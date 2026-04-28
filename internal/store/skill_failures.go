package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// SkillFailureContext is one (session_id, ts, body, nearby) tuple
// for a tool_failure that landed in the staleness-correlated window
// after the named skill was loaded. The `aichronicles skills evolve`
// command feeds these into the SkillRevision prompt so the LLM has
// concrete failure evidence to ground a revision in.
type SkillFailureContext struct {
	SessionID  string
	LoadTsMs   int64 // when the skill was loaded
	FailTsMs   int64 // when the tool_failure landed
	FailBody   string
	NearbyText string // concatenated content_text from a few events around the failure
}

// LoadSkillFailures returns up to `limit` (skill_load → tool_failure)
// pairs for the named skill within `windowMs` of the load. Each
// row carries the failure event's content + a tight window of
// nearby content_text (the previous + next 2 events in the same
// session) so the evolve prompt sees the failure-in-context, not
// just an isolated error string.
//
// Same window default as the staleness detector (10 min). Single
// query for the (load, fail) pairs; per-row follow-up for the
// nearby snippet so the join doesn't fan out.
func LoadSkillFailures(ctx context.Context, db *sql.DB, skill string, sinceMs, windowMs int64, limit int) ([]SkillFailureContext, error) {
	if windowMs <= 0 {
		windowMs = defaultStalenessWindow
	}
	if limit <= 0 {
		limit = 10
	}

	// Find every (load_event, fail_event) pair in the window.
	// LIMIT applies after sorting newest-first so the evolve
	// prompt sees the most-recent failures (the relevant ones
	// for "is this skill stale right now").
	const pairsQuery = `
SELECT x.session_id,
       e.ts_source_ms     AS load_ts,
       f.event_id          AS fail_event_id,
       f.ts_source_ms     AS fail_ts,
       COALESCE(f.content_text, '') AS fail_body
  FROM extractions x
  JOIN events e ON e.event_id = x.event_id
  JOIN events f ON f.session_id = x.session_id
                 AND f.kind = ?
                 AND f.ts_source_ms >  e.ts_source_ms
                 AND f.ts_source_ms <= e.ts_source_ms + ?
 WHERE x.kind = ? AND x.value = ? AND e.ts_source_ms >= ?
 ORDER BY f.ts_source_ms DESC
 LIMIT ?`

	rows, err := db.QueryContext(ctx, pairsQuery,
		ingest.KindToolFailure, windowMs,
		extract.KindSkillLoad, skill, sinceMs, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query skill failures: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type pair struct {
		sessionID, failEventID string
		loadTs, failTs         int64
		failBody               string
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.sessionID, &p.loadTs, &p.failEventID, &p.failTs, &p.failBody); err != nil {
			return nil, fmt.Errorf("scan pair: %w", err)
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Per-pair: pull events ±60s around the failure ts so the LLM
	// sees what the agent was trying to do when it failed. Cheap
	// because idx_events_session_ts covers (session_id, ts_source_ms).
	const nearbyQuery = `
SELECT kind, COALESCE(content_text, '') AS content
  FROM events
 WHERE session_id = ?
   AND ts_source_ms BETWEEN ? - 60000 AND ? + 60000
 ORDER BY ts_source_ms ASC
 LIMIT 6`

	out := make([]SkillFailureContext, 0, len(pairs))
	for _, p := range pairs {
		nrows, qerr := db.QueryContext(ctx, nearbyQuery, p.sessionID, p.failTs, p.failTs)
		if qerr != nil {
			return nil, fmt.Errorf("query nearby for %s: %w", p.sessionID, qerr)
		}
		var nearby []string
		for nrows.Next() {
			var kind, content string
			if err := nrows.Scan(&kind, &content); err != nil {
				_ = nrows.Close()
				return nil, fmt.Errorf("scan nearby: %w", err)
			}
			if content == "" {
				continue
			}
			// Cap each nearby snippet so a 100k tool_result can't
			// blow the prompt budget.
			const maxRunes = 600
			r := []rune(content)
			if len(r) > maxRunes {
				content = string(r[:maxRunes]) + "…"
			}
			nearby = append(nearby, fmt.Sprintf("[%s] %s", kind, content))
		}
		if err := nrows.Err(); err != nil {
			_ = nrows.Close()
			return nil, err
		}
		_ = nrows.Close()

		out = append(out, SkillFailureContext{
			SessionID:  p.sessionID,
			LoadTsMs:   p.loadTs,
			FailTsMs:   p.failTs,
			FailBody:   p.failBody,
			NearbyText: joinNearby(nearby),
		})
	}
	return out, nil
}

// joinNearby concatenates the per-event nearby snippets into one
// blob with a clear separator so the LLM can read them as a
// timeline rather than as one wall of text.
func joinNearby(snippets []string) string {
	if len(snippets) == 0 {
		return ""
	}
	var b strings.Builder
	for i, s := range snippets {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString(s)
	}
	return b.String()
}
