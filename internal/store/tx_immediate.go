package store

import (
	"context"
	"database/sql"
	"fmt"
)

// beginImmediate starts a transaction that already holds SQLite's
// write lock, for read-then-write maintenance operations.
//
// database/sql issues a plain deferred BEGIN, so a transaction that
// reads before it writes takes a WAL read snapshot first. If any
// other connection commits in between, the later write fails with
// SQLITE_BUSY_SNAPSHOT (517) — and SQLite deliberately does NOT
// invoke the busy handler for snapshot conflicts, so the DSN's
// busy_timeout(5000) does nothing and there is no retry.
//
// Prune and Scrub are both shaped exactly wrong for that: they count
// rows, then delete or rewrite them. Prune's window is short; Scrub's
// spans its entire scan of raw_envelopes, extractions and
// llm_outputs — seconds to minutes on a real corpus. Against a
// running daemon a single ingested event was enough to abort the
// whole thing, and it aborted AFTER the operator had watched the scan
// complete.
//
// Acquiring the write lock up front converts the failure mode into an
// ordinary lock wait, which busy_timeout DOES cover. The cost is that
// the daemon's writers block for the duration rather than racing and
// losing — the right trade for an operation the user explicitly
// invoked.
//
// Implemented by issuing a write immediately after BEGIN rather than
// by a DSN _txlock=immediate, which would make EVERY transaction on
// the shared pool a writer and serialise the ingest path.
func beginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// A no-op UPDATE against a row that always exists. It changes
	// nothing, but it makes SQLite take the write lock now, before
	// any read pins a snapshot we could be bumped off.
	if _, err := tx.ExecContext(ctx,
		`UPDATE meta SET value = value WHERE key = 'schema_version'`,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("acquire write lock: %w", err)
	}
	return tx, nil
}
