package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/toabctl/aichronicles/internal/ingest"
)

// IngestEnvelope writes a validated envelope through all three layers
// in the provided transaction. The caller is responsible for Validate(),
// supplying the original envelope bytes (so raw_envelopes holds the
// source of truth verbatim), and the server-side receipt timestamp.
//
// Returns deduped=true when raw_envelopes already had this event_id
// and no rows were written. Returns (false, err) on any SQL error.
// Cascading trigger work (sessions.event_count, events_fts) is handled
// by the schema's AFTER INSERT triggers.
func IngestEnvelope(tx *sql.Tx, env *ingest.Envelope, envelopeJSON []byte, tsServerMs int64) (deduped bool, err error) {
	if env == nil {
		return false, errors.New("IngestEnvelope: nil envelope")
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO raw_envelopes(
			event_id, ingest_seq, source_agent, source_session_id,
			ts_source_ms, ts_server_ms, envelope_json
		) VALUES (
			?, (SELECT COALESCE(MAX(ingest_seq), 0) + 1 FROM raw_envelopes),
			?, ?, ?, ?, ?
		)`,
		env.EventID, env.SourceAgent, env.SourceSessionID,
		env.TsSource.UnixMilli(), tsServerMs, string(envelopeJSON),
	)
	if err != nil {
		return false, fmt.Errorf("insert raw_envelope: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("raw insert rows affected: %w", err)
	}
	if n == 0 {
		return true, nil
	}

	sessionID := ingest.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	if _, err := tx.Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id)
		 VALUES (?, ?, ?)
		 ON CONFLICT DO NOTHING`,
		sessionID, env.SourceAgent, env.SourceSessionID,
	); err != nil {
		return false, fmt.Errorf("upsert session: %w", err)
	}

	var toolName, toolCallID sql.NullString
	if env.Tool != nil {
		toolName = nullString(env.Tool.Name)
		toolCallID = nullString(env.Tool.CallID)
	}
	if _, err := tx.Exec(
		`INSERT INTO events(
			event_id, session_id, source_agent, kind, role,
			ts_source_ms, cwd, tool_name, tool_call_id, content_text
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		env.EventID, sessionID, env.SourceAgent, env.Kind, nullString(env.Role),
		env.TsSource.UnixMilli(), nullString(env.Cwd), toolName, toolCallID, nullString(env.ContentText),
	); err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}

	return false, nil
}

// ResolveSessionID returns the derived session_id a caller would get
// from IngestEnvelope for a given (agent, source_session_id). Small
// convenience so callers can pre-compute without importing the ingest
// package directly.
func ResolveSessionID(sourceAgent, sourceSessionID string) string {
	return ingest.DeriveSessionID(sourceAgent, sourceSessionID)
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
