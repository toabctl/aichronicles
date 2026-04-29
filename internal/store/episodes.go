package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// DefaultEpisodeIdleGapMs is the inter-event gap above which the
// segmenter starts a new episode. Pink et al. (2026 —
// arXiv:2502.06975) leaves segmentation as RQ1 ("when and how to
// segment a continuous stream of agent experience into episodes");
// 10 minutes is the same window the staleness detector uses for
// "load → failure" correlation, so the two clocks agree on what
// counts as "tightly related." Operators can override via
// SegmentSession's idleGapMs argument.
const DefaultEpisodeIdleGapMs int64 = 10 * 60 * 1000

// MaxEpisodeIntentSummaryRunes caps the intent summary derived
// from an episode's first user_prompt. The summary is a quick
// orientation handle for retrieval, not a full prose record;
// 200 runes is enough for a recognisable opening line without
// blowing the table's storage footprint.
const MaxEpisodeIntentSummaryRunes = 200

// Episode is one row of the episodes table — a bounded,
// contextually-coherent run of events within one session. See
// migration 025 for the schema and the rationale; the Go struct
// mirrors it 1:1.
type Episode struct {
	ID            int64
	SessionID     string
	Ordinal       int
	StartedAtMs   int64
	EndedAtMs     int64
	Cwd           sql.NullString
	IntentSummary string
	EventCount    int
	FirstEventID  string
}

// episodeBoundary describes a segmenter decision point — a reason
// to close the current episode and start a new one. Documented as
// a small enum so the test suite can assert which trigger fired
// without introspecting on time deltas.
type episodeBoundary int

const (
	boundaryNone     episodeBoundary = iota
	boundaryFirst                    // synthetic: the very first event opens episode 1
	boundaryIdleGap                  // gap between consecutive events ≥ idleGapMs
	boundaryCwdShift                 // cwd transitioned to a new non-empty value
)

// SegmentSession walks an ordered slice of events from a single
// session and returns the episodes the segmenter would record.
// Pure function over the input — no DB writes, no I/O — so the
// caller decides whether to persist via SaveEpisodes.
//
// Ordering precondition: events MUST be sorted by ts_source_ms
// ASC (callers typically read via store.LoadSessionEvents, which
// already imposes this order). An out-of-order input is undefined
// behaviour; the function does not re-sort.
//
// idleGapMs ≤ 0 falls back to DefaultEpisodeIdleGapMs.
func SegmentSession(sessionID string, events []EventView, idleGapMs int64) []Episode {
	if sessionID == "" || len(events) == 0 {
		return nil
	}
	if idleGapMs <= 0 {
		idleGapMs = DefaultEpisodeIdleGapMs
	}

	type runState struct {
		startIdx int
		startMs  int64
		cwd      sql.NullString
		intent   string
		count    int
	}
	var (
		out    []Episode
		ord    = 1
		run    runState
		opened bool
	)
	flush := func(endMs int64) {
		out = append(out, Episode{
			SessionID:     sessionID,
			Ordinal:       ord,
			StartedAtMs:   run.startMs,
			EndedAtMs:     endMs,
			Cwd:           run.cwd,
			IntentSummary: run.intent,
			EventCount:    run.count,
			FirstEventID:  events[run.startIdx].EventID,
		})
		ord++
	}

	for i, e := range events {
		boundary := classifyBoundary(opened, run.cwd, &e, idleGapMs, prevTsAt(events, i))
		if boundary != boundaryNone && opened {
			flush(events[i-1].TsSourceMs)
			opened = false
		}
		if !opened {
			run = runState{
				startIdx: i,
				startMs:  e.TsSourceMs,
				cwd:      e.Cwd,
			}
			opened = true
		}
		// Update running cwd / intent if the current event upgrades
		// what we know.
		if !run.cwd.Valid && e.Cwd.Valid {
			run.cwd = e.Cwd
		}
		if run.intent == "" && e.Kind == ingest.KindUserPrompt && e.ContentText.Valid {
			run.intent = clipIntentSummary(e.ContentText.String)
		}
		run.count++
	}
	if opened {
		last := events[len(events)-1]
		flush(last.TsSourceMs)
	}
	return out
}

// EventView is enriched with Cwd in events.LoadSessionEvents but
// the existing struct (in events.go) lacks the field. The
// segmenter declares its own EventView-with-Cwd-and-FirstEventID
// here so it can stay decoupled from any future evolution of the
// shared struct. Callers populate this from the existing
// LoadSessionEvents result + a tiny adapter.

// classifyBoundary returns the trigger that should close the
// current episode (if any) before processing the next event. The
// caller still decides whether `opened` was true; this just names
// the rule.
func classifyBoundary(opened bool, runCwd sql.NullString, e *EventView, idleGapMs int64, prevTs int64) episodeBoundary {
	if !opened {
		return boundaryFirst
	}
	if prevTs > 0 && e.TsSourceMs-prevTs >= idleGapMs {
		return boundaryIdleGap
	}
	if e.Cwd.Valid && runCwd.Valid && e.Cwd.String != runCwd.String {
		return boundaryCwdShift
	}
	return boundaryNone
}

// prevTsAt returns events[i-1].TsSourceMs when i > 0, else 0.
// Wrapped as a helper so classifyBoundary stays a pure function
// over indexable inputs.
func prevTsAt(events []EventView, i int) int64 {
	if i <= 0 {
		return 0
	}
	return events[i-1].TsSourceMs
}

// clipIntentSummary collapses leading/trailing whitespace and
// returns at most MaxEpisodeIntentSummaryRunes runes. Single-line
// (no embedded newlines) so the rendering on /sessions stays
// uniform.
func clipIntentSummary(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= MaxEpisodeIntentSummaryRunes {
		return s
	}
	return string(r[:MaxEpisodeIntentSummaryRunes]) + "…"
}

// SaveEpisodes replaces the episodes for a session with the
// supplied slice in a single transaction. DELETE-then-INSERT
// keeps the operation idempotent: re-segmenting the same session
// produces the same boundaries (the segmenter is pure), and
// re-segmenting a session that gained more events at the end
// converges by replacing the tail episodes.
//
// Returns the number of inserted rows.
func SaveEpisodes(ctx context.Context, db *sql.DB, sessionID string, episodes []Episode) (int, error) {
	if sessionID == "" {
		return 0, errors.New("SaveEpisodes: session_id is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM episodes WHERE session_id = ?`, sessionID); err != nil {
		return 0, fmt.Errorf("delete existing: %w", err)
	}
	for _, ep := range episodes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO episodes(
				session_id, ordinal, started_at_ms, ended_at_ms,
				cwd, intent_summary, event_count, first_event_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, ep.Ordinal, ep.StartedAtMs, ep.EndedAtMs,
			ep.Cwd, ep.IntentSummary, ep.EventCount, ep.FirstEventID,
		); err != nil {
			return 0, fmt.Errorf("insert episode %d: %w", ep.Ordinal, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(episodes), nil
}

// LoadEpisodesBySession returns every episode for the given
// session in ordinal order. Empty slice (not error) when the
// session hasn't been segmented yet.
func LoadEpisodesBySession(ctx context.Context, db *sql.DB, sessionID string) ([]Episode, error) {
	if sessionID == "" {
		return nil, errors.New("LoadEpisodesBySession: session_id is required")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, session_id, ordinal, started_at_ms, ended_at_ms,
		        cwd, intent_summary, event_count, first_event_id
		   FROM episodes
		  WHERE session_id = ?
		  ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query episodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Episode
	for rows.Next() {
		var ep Episode
		if err := rows.Scan(&ep.ID, &ep.SessionID, &ep.Ordinal,
			&ep.StartedAtMs, &ep.EndedAtMs, &ep.Cwd, &ep.IntentSummary,
			&ep.EventCount, &ep.FirstEventID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}
