package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IngestPendingRow is one row of the ingest_pending staging table:
// a raw envelope POST body waiting for the worker to redact + extract
// + commit downstream. Mirrors the schema in migration 027 verbatim;
// the worker only ever needs ID + Body for processing, but EventID /
// AttemptCount come along so the worker can log and decide on retry
// caps without a second round-trip to SQLite.
type IngestPendingRow struct {
	ID           int64
	EventID      string
	Body         []byte
	ReceivedAtMs int64
	AttemptCount int
}

// EnqueuePending writes one pending row inside tx. Returns
// (id, deduped=false, nil) on a fresh insert, or
// (existingID, deduped=true, nil) when an earlier hook had already
// enqueued the same event_id. Phase-1 dedup means a retrying hook
// pays for one tiny INSERT-OR-IGNORE rather than re-running the full
// pipeline on the duplicate.
//
// The caller supplies tx so the daemon can compose this insert with
// any future bookkeeping (rate-limit counters, audit rows) in a
// single fsync. tsServerMs is the daemon's receipt clock and goes
// into received_at_ms; received_at_ms drives the worker's FIFO
// order and the index that backs it.
func EnqueuePending(ctx context.Context, tx *sql.Tx, eventID string, body []byte, tsServerMs int64) (int64, bool, error) {
	if eventID == "" {
		return 0, false, errors.New("EnqueuePending: empty event_id")
	}
	if len(body) == 0 {
		return 0, false, errors.New("EnqueuePending: empty body")
	}

	// Insert-or-ignore on the UNIQUE(event_id) constraint. INSERT
	// OR IGNORE keeps the statement single-round-trip and avoids
	// the SELECT-then-INSERT race two concurrent hooks would lose.
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO ingest_pending
		    (event_id, body, received_at_ms)
		VALUES (?, ?, ?)
	`, eventID, body, tsServerMs)
	if err != nil {
		return 0, false, fmt.Errorf("EnqueuePending: insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("EnqueuePending: rowsaffected: %w", err)
	}
	if n == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("EnqueuePending: lastinsertid: %w", err)
		}
		return id, false, nil
	}
	// Dedup branch: the prior row's id is what the caller can
	// reference in its 200 response. One follow-up SELECT — cheap
	// on a UNIQUE-indexed column.
	var existing int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM ingest_pending WHERE event_id = ?`, eventID,
	).Scan(&existing); err != nil {
		return 0, false, fmt.Errorf("EnqueuePending: lookup existing: %w", err)
	}
	return existing, true, nil
}

// PendingBatch returns the oldest `limit` pending rows, FIFO by
// received_at_ms. The worker calls this on every wake-up to feed
// its processing loop. limit caps memory: a daemon restart that
// finds a backlog of 50,000 rows should not allocate them all at
// once. 0 or negative limit returns no rows.
//
// The query reads ungranted by any lock — SQLite's WAL mode means
// readers don't block writers, so a fresh enqueue from a concurrent
// handler is visible on the worker's next batch.
func PendingBatch(ctx context.Context, db *sql.DB, limit int) ([]IngestPendingRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, event_id, body, received_at_ms, attempt_count
		  FROM ingest_pending
		 ORDER BY received_at_ms ASC, id ASC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("PendingBatch: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]IngestPendingRow, 0, limit)
	for rows.Next() {
		var r IngestPendingRow
		if err := rows.Scan(&r.ID, &r.EventID, &r.Body, &r.ReceivedAtMs, &r.AttemptCount); err != nil {
			return nil, fmt.Errorf("PendingBatch: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PendingBatch: rows: %w", err)
	}
	return out, nil
}

// MarkPendingProcessed deletes the row inside tx.
//
// CONSISTENCY MODEL: at-least-once, not exactly-once. In the
// current worker (internal/api/ingest_worker.go), Pipeline.Process
// commits the derived rows (raw_envelopes / events / extractions)
// in ITS OWN transaction, then MarkPendingProcessed runs in a
// SEPARATE tx to dequeue. A crash between those two commits
// leaves the row in ingest_pending; the next worker run re-invokes
// Pipeline.Process, which sees the existing raw_envelopes row via
// its UNIQUE(event_id) constraint and returns Result.Deduped=true.
// The dedup'd second pass falls through to MarkPendingProcessed
// again and finally drains the row. SSE is suppressed on the
// dedup pass (worker checks result.Deduped) so consumers don't
// double-render.
//
// Earlier doc strings here claimed "same transaction"; that was
// aspirational, not implemented (arch_review_2026_05_13 MEDIUM
// #6). True same-tx semantics would require Pipeline.Process to
// take an explicit tx argument; left as an architectural
// follow-up since the at-least-once + raw_envelopes dedup path
// already gives the durability invariant the worker relies on.
//
// id is what PendingBatch returned, not event_id — using the
// primary key keeps the DELETE single-row even if a future schema
// allows multiple pending attempts per event_id.
func MarkPendingProcessed(ctx context.Context, tx *sql.Tx, id int64) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM ingest_pending WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("MarkPendingProcessed: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("MarkPendingProcessed: rowsaffected: %w", err)
	}
	if n != 1 {
		// Zero rows affected means the row was already deleted
		// (race with a concurrent worker, or a manual cleanup).
		// Not an error per se, but surface it so a misbehaving
		// pair of workers shows up in tests.
		return fmt.Errorf("MarkPendingProcessed: row %d not found", id)
	}
	return nil
}

// MarkPendingFailed bumps attempt_count, stores the short error
// string and the current receipt time, and leaves the row for a
// later retry. Runs in its own transaction (no tx argument)
// because the worker calls it AFTER rolling back the doomed
// processing tx — the failure record must survive that rollback.
//
// The error message is truncated to 512 chars: SQLite handles
// long TEXT fine, but a multi-MB redactor error trace pinned in
// a queue row is operational noise.
func MarkPendingFailed(ctx context.Context, db *sql.DB, id int64, tsServerMs int64, errMsg string) error {
	if len(errMsg) > 512 {
		errMsg = errMsg[:512]
	}
	_, err := db.ExecContext(ctx, `
		UPDATE ingest_pending
		   SET attempt_count = attempt_count + 1,
		       last_attempt_at_ms = ?,
		       last_error = ?
		 WHERE id = ?
	`, tsServerMs, errMsg, id)
	if err != nil {
		return fmt.Errorf("MarkPendingFailed: update: %w", err)
	}
	return nil
}

// CountPending returns the current backlog size. Drives the
// daemon's backpressure decision: when CountPending exceeds the
// configured cap, handleIngest returns 503 to the hook (which
// then takes its outage path) instead of accepting an envelope
// it can't drain.
//
// O(1) thanks to SQLite's COUNT(*) on a no-WHERE query (uses the
// table's row count directly when no filter is present).
func CountPending(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingest_pending`).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountPending: scan: %w", err)
	}
	return n, nil
}
