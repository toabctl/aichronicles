package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ProjectAggregate is one row of /projects. Cwd is the start cwd
// of every session in the bucket — the caller (typically a web
// handler) rolls these up to a project root via
// skills.FindProjectRootGeneric so several nearby cwds collapse
// into one group.
//
// LastActivityMs is the most recent ended_at_ms (or started_at_ms
// when ended is NULL) across the bucket — gives the page a
// natural "newest first" sort axis.
type ProjectAggregate struct {
	Cwd            string
	Sessions       int
	Events         int
	LastActivityMs int64
}

// LoadProjectAggregates returns one row per distinct start cwd
// within the window, with per-cwd session/event counts and the
// most recent activity timestamp. Caller groups these by project
// root.
//
// Reads sessions.start_cwd directly (added in migration 015) —
// the previous implementation re-derived it via a 9-line CTE on
// every call, which is the same value the trigger now maintains.
func LoadProjectAggregates(ctx context.Context, db *sql.DB, sinceMs int64) ([]ProjectAggregate, error) {
	const q = `
SELECT s.start_cwd                                          AS cwd,
       COUNT(*)                                             AS sessions,
       COALESCE(SUM(s.event_count), 0)                      AS events,
       MAX(COALESCE(s.ended_at_ms, s.started_at_ms, 0))     AS last_activity_ms
  FROM sessions s
 WHERE s.start_cwd IS NOT NULL
   AND COALESCE(s.ended_at_ms, s.started_at_ms, 0) >= ?
 GROUP BY s.start_cwd
 ORDER BY last_activity_ms DESC`
	rows, err := db.QueryContext(ctx, q, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("query project aggregates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProjectAggregate
	for rows.Next() {
		var p ProjectAggregate
		if err := rows.Scan(&p.Cwd, &p.Sessions, &p.Events, &p.LastActivityMs); err != nil {
			return nil, fmt.Errorf("scan project aggregate: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
