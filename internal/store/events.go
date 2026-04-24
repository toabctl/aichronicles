package store

import (
	"context"
	"database/sql"
	"fmt"
)

// EventView is the read-only shape used by code that walks stored
// events (prompt builders, export, audit). Nullable fields are
// modelled with sql.Null* so callers can distinguish "empty string"
// from "column was NULL".
type EventView struct {
	EventID     string
	Kind        string
	Role        sql.NullString
	ContentText sql.NullString
	TsSourceMs  int64
	ToolName    sql.NullString
}

// SessionDigestRow is the read shape used by reflect/propose. Each
// row represents one conversation: its time window, the first user
// prompt (so the model has something to anchor on when there is no
// summary yet), and the latest llm_outputs summary if any.
//
// Callers fold this into the prompts.SessionDigest payload; keeping
// the DB-facing shape separate lets us evolve either side without
// coupling.
type SessionDigestRow struct {
	ID            string
	StartedAtMs   sql.NullInt64
	EndedAtMs     sql.NullInt64
	Cwd           sql.NullString
	FirstPrompt   sql.NullString
	LatestSummary sql.NullString
}

// LoadRecentSessionDigests returns the most-recently-ended sessions
// whose ended_at is within the `sinceMs` cutoff, newest first,
// capped at `limit`. Sessions with a NULL ended_at fall back to
// started_at so mid-flight captures aren't invisible.
//
// Both subqueries (first_prompt, latest_summary) are correlated;
// at thousand-session scale the cost is negligible. If that ever
// becomes the slow spot, materialize them into columns.
func LoadRecentSessionDigests(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]SessionDigestRow, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := db.QueryContext(ctx,
		`SELECT s.id, s.started_at_ms, s.ended_at_ms, s.cwd,
			(SELECT content_text FROM events
				WHERE session_id = s.id AND kind = 'user_prompt'
				ORDER BY ts_source_ms ASC LIMIT 1) AS first_prompt,
			(SELECT body FROM llm_outputs
				WHERE session_id = s.id AND kind = 'summary'
				ORDER BY created_at_ms DESC LIMIT 1) AS latest_summary
		FROM sessions s
		WHERE COALESCE(s.ended_at_ms, s.started_at_ms, 0) >= ?
		ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
		LIMIT ?`,
		sinceMs, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query session digests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionDigestRow
	for rows.Next() {
		var r SessionDigestRow
		if err := rows.Scan(&r.ID, &r.StartedAtMs, &r.EndedAtMs, &r.Cwd, &r.FirstPrompt, &r.LatestSummary); err != nil {
			return nil, fmt.Errorf("scan digest: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DefaultEventsPerSessionLimit caps LoadEventsForSession's result set
// when the caller does not supply a tighter bound. 10k events per
// session is generous (an hour of hot tool use is typically under 1k
// events) and keeps a pathological session from loading 1M rows into
// memory for a summarize call.
const DefaultEventsPerSessionLimit = 10_000

// LoadEventsForSession returns up to `limit` events for a session,
// oldest first. An empty slice is returned for an unknown session.
// A non-positive `limit` uses DefaultEventsPerSessionLimit — callers
// that truly want "every event" should pass a very large number and
// own the memory consequences.
func LoadEventsForSession(ctx context.Context, db *sql.DB, sessionID string, limit int) ([]EventView, error) {
	if limit <= 0 {
		limit = DefaultEventsPerSessionLimit
	}
	rows, err := db.QueryContext(ctx,
		`SELECT event_id, kind, role, content_text, ts_source_ms, tool_name
		 FROM events
		 WHERE session_id = ?
		 ORDER BY ts_source_ms ASC, rowid ASC
		 LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventView
	for rows.Next() {
		var e EventView
		if err := rows.Scan(&e.EventID, &e.Kind, &e.Role, &e.ContentText, &e.TsSourceMs, &e.ToolName); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
