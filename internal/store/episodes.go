package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/internal/events"
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
func SegmentSession(sessionID string, evs []events.EventView, idleGapMs int64) []events.Episode {
	if sessionID == "" || len(evs) == 0 {
		return nil
	}
	if idleGapMs <= 0 {
		idleGapMs = DefaultEpisodeIdleGapMs
	}

	type runState struct {
		startIdx int
		startMs  int64
		cwd      events.NullString
		intent   string
		count    int
	}
	var (
		out    []events.Episode
		ord    = 1
		run    runState
		opened bool
	)
	flush := func(endMs int64) {
		out = append(out, events.Episode{
			SessionID:     sessionID,
			Ordinal:       ord,
			StartedAtMs:   run.startMs,
			EndedAtMs:     endMs,
			Cwd:           run.cwd,
			IntentSummary: run.intent,
			EventCount:    run.count,
			FirstEventID:  evs[run.startIdx].EventID,
		})
		ord++
	}

	for i, e := range evs {
		boundary := classifyBoundary(opened, run.cwd, &e, idleGapMs, prevTsAt(evs, i))
		if boundary != boundaryNone && opened {
			flush(evs[i-1].TsSourceMs)
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
		if run.intent == "" && e.Kind == events.KindUserPrompt && e.ContentText.Valid {
			run.intent = clipIntentSummary(e.ContentText.String)
		}
		run.count++
	}
	if opened {
		last := evs[len(evs)-1]
		flush(last.TsSourceMs)
	}
	return out
}

// events.EventView is enriched with Cwd in events.LoadSessionEvents but
// the existing struct (in events.go) lacks the field. The
// segmenter declares its own events.EventView-with-Cwd-and-FirstEventID
// here so it can stay decoupled from any future evolution of the
// shared struct. Callers populate this from the existing
// LoadSessionEvents result + a tiny adapter.

// classifyBoundary returns the trigger that should close the
// current episode (if any) before processing the next event. The
// caller still decides whether `opened` was true; this just names
// the rule.
func classifyBoundary(opened bool, runCwd events.NullString, e *events.EventView, idleGapMs int64, prevTs int64) episodeBoundary {
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
func prevTsAt(evs []events.EventView, i int) int64 {
	if i <= 0 {
		return 0
	}
	return evs[i-1].TsSourceMs
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
func SaveEpisodes(ctx context.Context, db *sql.DB, sessionID string, episodes []events.Episode) (int, error) {
	if sessionID == "" {
		return 0, errors.New("SaveEpisodes: session_id is required")
	}
	if err := WithTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM episodes WHERE session_id = ?`, sessionID); err != nil {
			return fmt.Errorf("delete existing: %w", err)
		}
		for _, ep := range episodes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO episodes(
					session_id, ordinal, started_at_ms, ended_at_ms,
					cwd, intent_summary, event_count, first_event_id
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				sessionID, ep.Ordinal, ep.StartedAtMs, ep.EndedAtMs,
				eventsNullStringToSQL(ep.Cwd),
				ep.IntentSummary, ep.EventCount, ep.FirstEventID,
			); err != nil {
				return fmt.Errorf("insert episode %d: %w", ep.Ordinal, err)
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(episodes), nil
}

// FindEpisodesOpts narrows a FindEpisodes query. All fields are
// optional; an opts-zero call returns the most recent episodes
// across the corpus, capped at Limit (or DefaultFindEpisodesLimit
// when Limit ≤ 0).
//
// Pink et al. (2026 — arXiv:2502.06975) frame episodic memory as
// instance-specific, contextually-bound recall. The natural query
// surface for an agent doing recall is therefore:
//
//   - SessionID   → "show me the episodes within session X" (after
//     a session id surfaced via list_sessions)
//   - Cwd         → "episodes in this project" (most common
//     practical filter when reopening a project)
//   - QueryContains → case-insensitive substring on intent_summary,
//     which is the first user prompt — the natural
//     human handle for "the time I did Y"
//   - SinceMs     → recency window (older episodes age out as the
//     active context shifts)
//
// QueryContains does NOT use FTS5: intent_summary is a thin 200-rune
// field by design, and a LIKE substring scan over the episodes table
// is faster and more predictable than maintaining a parallel FTS index
// for a column that's already small. If the agent wants to search
// inside the episode's events, it pivots through search_events with
// the returned session_id + time window.
type FindEpisodesOpts struct {
	SessionID     string
	Cwd           string
	QueryContains string
	SinceMs       int64
	Limit         int
}

// DefaultFindEpisodesLimit caps a FindEpisodes call when the
// caller doesn't specify one. 50 is the same envelope list_sessions
// uses — enough that an agent's recall query lands on the right
// row without scrolling, small enough that the response body stays
// in MCP's text budget.
const DefaultFindEpisodesLimit = 50

// FindEpisodes runs the recall query described by opts and returns
// matching rows ordered by ended_at_ms DESC (most-recent first). An
// empty result is NOT an error — it's the normal "no episodes match
// this filter" outcome.
func FindEpisodes(ctx context.Context, db *sql.DB, opts FindEpisodesOpts) ([]events.Episode, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultFindEpisodesLimit
	}

	var (
		filter strings.Builder
		args   []any
	)
	if opts.SessionID != "" {
		filter.WriteString(` AND session_id = ?`)
		args = append(args, opts.SessionID)
	}
	if opts.Cwd != "" {
		filter.WriteString(` AND cwd = ?`)
		args = append(args, opts.Cwd)
	}
	if opts.SinceMs > 0 {
		filter.WriteString(` AND ended_at_ms >= ?`)
		args = append(args, opts.SinceMs)
	}
	if q := strings.TrimSpace(opts.QueryContains); q != "" {
		filter.WriteString(` AND lower(intent_summary) LIKE ?`)
		// `%` wildcards on both sides → substring; lower() on both
		// sides for case-insensitive match.
		args = append(args, "%"+strings.ToLower(q)+"%")
	}
	args = append(args, limit)

	// `, id DESC` is the tiebreaker for ms-collisions. Episodes
	// with identical ended_at_ms (single-event episodes pinned to
	// the same wall clock, or seeded fixtures with hardcoded ts)
	// would otherwise return in engine-defined order — visible as
	// flaky pagination and non-deterministic LIMIT'd recall.
	q := `SELECT ` + episodeColumns + `
	        FROM episodes
	       WHERE 1=1` + filter.String() + `
	       ORDER BY ended_at_ms DESC, id DESC
	       LIMIT ?`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query episodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []events.Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// LoadSessionsNeedingSegmentation returns the IDs of sessions whose
// episodes table is out-of-date with their events table — either
// the session was never segmented (no episodes rows) OR new events
// have arrived since segmentation last ran (events with
// ts_source_ms > MAX(episodes.ended_at_ms)).
//
// Pre-fix, episode segmentation lived inside the induction sweep's
// per-candidate loop, which was gated on `NOT EXISTS llm_outputs
// kind=induction`. Once a session was inducted it dropped out of
// the candidate set and never got re-segmented, so any late event
// arriving after induction created silent episode/event drift. A
// dedicated pre-loop pass keyed on episode staleness (not induction
// state) closes that gap.
//
// Eligibility:
//   - sessions.event_count >= minEvents (skip trivial sessions)
//   - COALESCE(ended_at_ms, started_at_ms) <= idleCutoff (give the
//     session time to settle; mid-session segmentation creates
//     noisy boundaries that the next pass would just rewrite)
//   - either no episodes rows OR an event newer than the latest
//     episode's ended_at_ms exists for this session
//
// Ordered newest-first so a sweep with a small `limit` catches
// recently-active sessions before historical backfill.
func LoadSessionsNeedingSegmentation(ctx context.Context, db *sql.DB, idleCutoffMs, idleMs int64, minEvents, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `
WITH ep AS (
    SELECT session_id, MAX(ended_at_ms) AS max_end
      FROM episodes
     GROUP BY session_id
)
SELECT s.id
  FROM sessions s
  LEFT JOIN ep ON ep.session_id = s.id
 WHERE s.event_count >= ?
   AND COALESCE(s.ended_at_ms, s.started_at_ms) IS NOT NULL
   AND COALESCE(s.ended_at_ms, s.started_at_ms) <= ?
   AND (
        ep.session_id IS NULL
     OR EXISTS (
            SELECT 1 FROM events ev
             WHERE ev.session_id = s.id
               AND ev.ts_source_ms > ep.max_end
        )
   )
 ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms) DESC
 LIMIT ?`
	cutoff := idleCutoffMs - idleMs
	rows, err := db.QueryContext(ctx, q, minEvents, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query needing-segmentation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LoadEpisodesBySession returns every episode for the given
// session in ordinal order. Empty slice (not error) when the
// session hasn't been segmented yet.
func LoadEpisodesBySession(ctx context.Context, db *sql.DB, sessionID string) ([]events.Episode, error) {
	if sessionID == "" {
		return nil, errors.New("LoadEpisodesBySession: session_id is required")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+episodeColumns+`
		   FROM episodes
		  WHERE session_id = ?
		  ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query episodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []events.Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// episodeColumns is the canonical column list for SELECTs that feed
// scanEpisode. Keep this string and the scan helper in lockstep.
const episodeColumns = `id, session_id, ordinal, started_at_ms, ended_at_ms,
	cwd, intent_summary, event_count, first_event_id`

// scanEpisode scans one row in episodeColumns order. The cwd column
// is the only nullable in the projection; intent_summary is empty-
// string when absent rather than NULL.
func scanEpisode(rows *sql.Rows) (events.Episode, error) {
	var (
		ep  events.Episode
		cwd sql.NullString
	)
	if err := rows.Scan(&ep.ID, &ep.SessionID, &ep.Ordinal,
		&ep.StartedAtMs, &ep.EndedAtMs, &cwd, &ep.IntentSummary,
		&ep.EventCount, &ep.FirstEventID); err != nil {
		return events.Episode{}, fmt.Errorf("scan: %w", err)
	}
	ep.Cwd = nullStringToEvents(cwd)
	return ep, nil
}
