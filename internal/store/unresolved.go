package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UnresolvedItem is one entry from a prior session's
// summary.unresolved list, paired with enough provenance for the
// reader to jump back to the source. Distinct from a raw
// SummaryResult.Unresolved string because the consumer needs to
// know WHEN this came up — a 5-day-old "should we add tests"
// reads differently from one from this morning.
type UnresolvedItem struct {
	SessionID    string
	SessionShort string // first 8 chars of SessionID for compact rendering
	EndedAtMs    int64  // 0 when the session is still active
	Topic        string // session.summary.topic — gives the item context
	Item         string // the actual unresolved string
}

// LoadUnresolvedForCwd returns unresolved items from prior sessions
// in the given cwd, newest-session first. One UnresolvedItem per
// item (not per session) so a session with three unresolved
// threads contributes three rows.
//
// Filters:
//
//   - cwd: exact match. v1 keeps it tight; a fuzzy/prefix match
//     would let stale items from an unrelated subtree surface in
//     the wrong project. The user can rerun with the right cwd
//     when they need to.
//
//   - sinceMs: lower bound on ended_at_ms (or started_at_ms when
//     end is NULL). Defaults to 30 days when ≤0 — far enough
//     back to catch a thread that paused for two weeks, recent
//     enough to drop ancient TODOs that the user has long since
//     resolved or forgotten.
//
//   - maxSessions: cap on how many prior sessions feed the
//     output. Prevents a chatty cwd with hundreds of sessions
//     from drowning the SessionStart hook in noise. Defaults to
//     5 when ≤0.
//
//   - maxItemsPerSession: cap on items pulled from one session.
//     The model can emit lots of unresolved items per summary;
//     we want the most-load-bearing few. Defaults to 5 when ≤0.
//
// The query joins llm_outputs to find the LATEST kind='summary'
// row per session, parses $.unresolved out of the JSON body in
// Go (rather than SQL), and emits one row per item. Sessions
// without a summary or with an empty unresolved list contribute
// nothing.
func LoadUnresolvedForCwd(
	ctx context.Context,
	db *sql.DB,
	cwd string,
	sinceMs int64,
	maxSessions, maxItemsPerSession int,
) ([]UnresolvedItem, error) {
	if cwd == "" {
		return nil, nil
	}
	if sinceMs <= 0 {
		sinceMs = time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	}
	if maxSessions <= 0 {
		maxSessions = 5
	}
	if maxItemsPerSession <= 0 {
		maxItemsPerSession = 5
	}

	// "Latest summary per session" via a small CTE that picks
	// MAX(id) GROUPed by session_id. Replaces the previous
	// correlated-IN subquery (`o.id IN (SELECT MAX(id) WHERE
	// session_id = s.id GROUP BY session_id)`), which
	// referenced the outer `s.id` once per row. The CTE form
	// pre-aggregates one pass over llm_outputs and then joins,
	// which the SQLite planner can flatten into a single scan
	// (idx_llm_outputs_session_kind_created covers the
	// kind/session predicate). Behaviour-equivalent.
	q := `
		WITH latest AS (
		  SELECT session_id, MAX(id) AS llm_output_id
		    FROM llm_outputs
		   WHERE kind = ?
		   GROUP BY session_id
		)
		SELECT s.id,
		       COALESCE(s.ended_at_ms, 0),
		       o.body
		  FROM sessions s
		  JOIN latest      l ON l.session_id = s.id
		  JOIN llm_outputs o ON o.id        = l.llm_output_id
		 WHERE s.cwd = ?
		   AND ` + EffectiveTsExpr + ` >= ?
		 ORDER BY ` + EffectiveTsExpr + ` DESC
		 LIMIT ?
	`
	rows, err := db.QueryContext(ctx, q,
		string(LLMKindSummary), cwd, sinceMs, maxSessions)
	if err != nil {
		return nil, fmt.Errorf("query unresolved: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Inline struct mirrors the slice of fields BuildSummary's
	// SummaryResult has — kept local so unresolved.go doesn't
	// import pkg/llm/prompts (which would induce a cycle when
	// the prompts package eventually imports back into store).
	type summaryShape struct {
		Topic      string   `json:"topic"`
		Unresolved []string `json:"unresolved"`
	}

	var out []UnresolvedItem
	for rows.Next() {
		var (
			id      string
			endedMs int64
			body    string
		)
		if err := rows.Scan(&id, &endedMs, &body); err != nil {
			return nil, fmt.Errorf("scan unresolved row: %w", err)
		}
		var parsed summaryShape
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			// Bad cached body shouldn't kill the query — skip the
			// session. (LoadLatestSummary in the web layer also
			// tolerates this; same contract here.)
			continue
		}
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		count := 0
		for _, item := range parsed.Unresolved {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if count >= maxItemsPerSession {
				break
			}
			out = append(out, UnresolvedItem{
				SessionID:    id,
				SessionShort: short,
				EndedAtMs:    endedMs,
				Topic:        parsed.Topic,
				Item:         item,
			})
			count++
		}
	}
	return out, rows.Err()
}
