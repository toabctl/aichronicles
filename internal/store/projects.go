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
// The "start cwd" is the first event's cwd (matching what the
// resume button uses); we don't use sessions.cwd directly because
// the trigger keeps that as the LAST cwd, which conflates project
// boundaries when the user cd's mid-session.
func LoadProjectAggregates(ctx context.Context, db *sql.DB, sinceMs int64) ([]ProjectAggregate, error) {
	const q = `
WITH first_event AS (
    SELECT e.session_id,
           MIN(e.ts_source_ms) AS ts
      FROM events e
      JOIN sessions s ON s.id = e.session_id
     WHERE COALESCE(s.ended_at_ms, s.started_at_ms, 0) >= ?
       AND e.cwd IS NOT NULL
     GROUP BY e.session_id
),
session_cwd AS (
    SELECT fe.session_id, e.cwd
      FROM first_event fe
      JOIN events e
        ON e.session_id = fe.session_id AND e.ts_source_ms = fe.ts
     WHERE e.cwd IS NOT NULL
)
SELECT sc.cwd                                              AS cwd,
       COUNT(DISTINCT sc.session_id)                       AS sessions,
       COALESCE(SUM(s.event_count), 0)                     AS events,
       MAX(COALESCE(s.ended_at_ms, s.started_at_ms, 0))    AS last_activity_ms
  FROM session_cwd sc
  JOIN sessions s ON s.id = sc.session_id
 GROUP BY sc.cwd
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
