package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/events/extract"
)

// ErrRedactionRequired is returned when an envelope reaches the store
// without Redaction.Applied=true. It is the last line of defense for
// Block A's "no unredacted secrets in the DB" invariant: both the
// daemon HTTP handler and the in-process import commands end up here,
// so enforcing it at this choke point means no future code path can
// quietly bypass redaction by calling IngestEnvelope directly.
var ErrRedactionRequired = errors.New("IngestEnvelope: envelope.redaction.applied must be true")

// IngestEnvelope writes a validated envelope through all three layers
// in the provided transaction. The caller is responsible for Validate(),
// supplying the original envelope bytes (so raw_envelopes holds the
// source of truth verbatim), and the server-side receipt timestamp.
//
// ctx is propagated to every SQL call so an HTTP request cancellation
// or a daemon shutdown can abort a long write cleanly.
//
// Returns deduped=true when raw_envelopes already had this event_id
// and no rows were written. Returns (false, err) on any SQL error.
// Returns (false, ErrRedactionRequired) if env.Redaction.Applied is
// not explicitly true.
// Cascading trigger work (sessions.event_count, events_fts) is handled
// by the schema's AFTER INSERT triggers.
func IngestEnvelope(ctx context.Context, tx *sql.Tx, env *events.Envelope, envelopeJSON []byte, tsServerMs int64) (deduped bool, err error) {
	if env == nil {
		return false, errors.New("IngestEnvelope: nil envelope")
	}
	if env.Redaction == nil || !env.Redaction.Applied {
		return false, ErrRedactionRequired
	}

	// Race-free ingest_seq allocation: claim the next value via
	// UPDATE...RETURNING on the seq table (migration 008). The
	// UPDATE acquires SQLite's write lock; concurrent transactions
	// serialise here, each getting a unique value. The previous
	// MAX(ingest_seq)+1 sub-query was racy under SetMaxOpenConns>1
	// and could lose events to a UNIQUE constraint violation that
	// INSERT OR IGNORE silently swallowed.
	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE seq SET next_value = next_value + 1
		  WHERE name = 'ingest_seq'
		  RETURNING next_value - 1`,
	).Scan(&nextSeq); err != nil {
		return false, fmt.Errorf("allocate ingest_seq: %w", err)
	}

	// INSERT OR IGNORE on event_id collision (the dedup path —
	// real duplicate envelopes from upstream retries). Note we
	// already paid for one ingest_seq value in the seq counter
	// above; on duplicate the value is "burned" but the resulting
	// gap is harmless (ingest_seq is monotonic but not required to
	// be contiguous).
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO raw_envelopes(
			event_id, ingest_seq, source_agent, source_session_id,
			ts_source_ms, ts_server_ms, envelope_json
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		env.EventID, nextSeq, env.SourceAgent, env.SourceSessionID,
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
		// Event_id collision — real duplicate from upstream retry.
		// Distinct from the silent-loss path the previous
		// implementation could fall into.
		return true, nil
	}

	sessionID := events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	if _, err := tx.ExecContext(ctx,
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
	var subagentID, subagentType sql.NullString
	if env.Subagent != nil {
		subagentID = nullString(env.Subagent.ID)
		subagentType = nullString(env.Subagent.Type)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(
			event_id, session_id, source_agent, kind, role,
			ts_source_ms, cwd, tool_name, tool_call_id, content_text,
			subagent_id, subagent_type, transport,
			source_agent_version, host
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		env.EventID, sessionID, env.SourceAgent, env.Kind, nullString(env.Role),
		env.TsSource.UnixMilli(), nullString(env.Cwd), toolName, toolCallID, nullString(env.ContentText),
		subagentID, subagentType, env.Transport,
		env.SourceAgentVersion, env.Host,
	); err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}

	// Extractions: URLs, file paths, shell commands. Best-effort and
	// synchronous — they run in the same transaction so the row set
	// stays consistent with events. Extractor bugs should not fail
	// ingest, so a malformed extra_json falls back to NULL rather
	// than aborting; but a SQL insert error is structural and does
	// propagate up to the caller.
	for _, ex := range extract.FromEnvelope(env) {
		var extraJSON sql.NullString
		if len(ex.Extra) > 0 {
			if b, err := json.Marshal(ex.Extra); err == nil {
				extraJSON = sql.NullString{String: string(b), Valid: true}
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO extractions(event_id, session_id, kind, value, extra_json) VALUES (?, ?, ?, ?, ?)`,
			env.EventID, sessionID, ex.Kind, ex.Value, extraJSON,
		); err != nil {
			return false, fmt.Errorf("insert extraction (%s=%q): %w", ex.Kind, ex.Value, err)
		}
	}

	return false, nil
}

// ResolveSessionID returns the derived session_id a caller would get
// from IngestEnvelope for a given (agent, source_session_id). Small
// convenience so callers can pre-compute without importing the ingest
// package directly.
func ResolveSessionID(sourceAgent, sourceSessionID string) string {
	return events.DeriveSessionID(sourceAgent, sourceSessionID)
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
