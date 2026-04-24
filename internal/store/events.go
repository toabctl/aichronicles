package store

import (
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

// LoadEventsForSession returns every event for a session, oldest
// first. An empty slice is returned for an unknown session.
func LoadEventsForSession(db *sql.DB, sessionID string) ([]EventView, error) {
	rows, err := db.Query(
		`SELECT event_id, kind, role, content_text, ts_source_ms, tool_name
		 FROM events
		 WHERE session_id = ?
		 ORDER BY ts_source_ms ASC, rowid ASC`,
		sessionID,
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
