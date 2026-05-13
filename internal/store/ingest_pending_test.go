package store

import (
	"bytes"
	"database/sql"
	"testing"
)

// enqueueOne is a small helper that hides the WithTx ceremony. The
// pending API is tx-scoped on purpose (so handleIngest can compose
// the enqueue with adjacent bookkeeping in one fsync), but most
// tests just want to drop a row in and inspect the result.
func enqueueOne(t *testing.T, s *Store, eventID string, body []byte, ts int64) (int64, bool) {
	t.Helper()
	var (
		id      int64
		deduped bool
	)
	if err := WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		var err error
		id, deduped, err = EnqueuePending(t.Context(), tx, eventID, body, ts)
		return err
	}); err != nil {
		t.Fatalf("enqueue %s: %v", eventID, err)
	}
	return id, deduped
}

func TestEnqueuePending_FreshInsertReturnsIDAndDedupFalse(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	id, deduped := enqueueOne(t, s, "evt-1", []byte("body-1"), 1000)
	if id <= 0 {
		t.Errorf("expected positive id, got %d", id)
	}
	if deduped {
		t.Error("fresh insert should not be marked deduped")
	}
}

func TestEnqueuePending_SecondInsertSameEventIDDedups(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	firstID, dedup1 := enqueueOne(t, s, "evt-dup", []byte("body-1"), 1000)
	if dedup1 {
		t.Fatal("first insert should not be deduped")
	}
	secondID, dedup2 := enqueueOne(t, s, "evt-dup", []byte("body-2"), 2000)
	if !dedup2 {
		t.Error("second insert with same event_id should be deduped")
	}
	if secondID != firstID {
		t.Errorf("dedup must return the original row id; got %d, want %d", secondID, firstID)
	}

	// Body must not have been overwritten — the FIRST envelope is
	// the one the worker will eventually process; a retrying hook
	// shouldn't get to swap the bytes underneath us.
	rows, err := PendingBatch(t.Context(), s.DB(), 10)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %d", len(rows))
	}
	if !bytes.Equal(rows[0].Body, []byte("body-1")) {
		t.Errorf("dedup overwrote body; got %q, want %q", rows[0].Body, "body-1")
	}
}

func TestEnqueuePending_RejectsEmptyEventID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	if err := WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		_, _, err := EnqueuePending(t.Context(), tx, "", []byte("body"), 1)
		return err
	}); err == nil {
		t.Error("empty event_id should error")
	}
}

func TestEnqueuePending_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	if err := WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		_, _, err := EnqueuePending(t.Context(), tx, "evt", nil, 1)
		return err
	}); err == nil {
		t.Error("empty body should error")
	}
}

func TestPendingBatch_FIFOByReceivedAt(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Insert out-of-order ts to confirm the index drives ordering,
	// not insertion order.
	enqueueOne(t, s, "evt-c", []byte("body-c"), 3000)
	enqueueOne(t, s, "evt-a", []byte("body-a"), 1000)
	enqueueOne(t, s, "evt-b", []byte("body-b"), 2000)

	rows, err := PendingBatch(t.Context(), s.DB(), 10)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	want := []string{"evt-a", "evt-b", "evt-c"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].EventID != w {
			t.Errorf("row %d: got %q, want %q", i, rows[i].EventID, w)
		}
	}
}

func TestPendingBatch_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	for i := range 5 {
		enqueueOne(t, s, "evt-"+string(rune('a'+i)), []byte("body"), int64(i+1)*1000)
	}
	rows, err := PendingBatch(t.Context(), s.DB(), 3)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows, want 3", len(rows))
	}
}

func TestPendingBatch_ZeroLimitReturnsNoRows(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	enqueueOne(t, s, "evt", []byte("body"), 1)

	rows, err := PendingBatch(t.Context(), s.DB(), 0)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("zero limit should return no rows; got %d", len(rows))
	}
}

func TestMarkPendingProcessed_RemovesRow(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	id, _ := enqueueOne(t, s, "evt", []byte("body"), 1000)

	if err := WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		return MarkPendingProcessed(t.Context(), tx, id)
	}); err != nil {
		t.Fatalf("MarkPendingProcessed: %v", err)
	}
	n, err := CountPending(t.Context(), s.DB())
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 0 {
		t.Errorf("expected empty backlog after process, got %d", n)
	}
}

func TestMarkPendingProcessed_MissingRowErrors(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	err := WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		return MarkPendingProcessed(t.Context(), tx, 999)
	})
	if err == nil {
		t.Error("processing a non-existent row should error so a double-process is loud")
	}
}

func TestMarkPendingFailed_BumpsAttemptAndRecordsError(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	id, _ := enqueueOne(t, s, "evt", []byte("body"), 1000)

	if err := MarkPendingFailed(t.Context(), s.DB(), id, 2000, "redact: boom"); err != nil {
		t.Fatalf("MarkPendingFailed: %v", err)
	}
	rows, err := PendingBatch(t.Context(), s.DB(), 10)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("row should still be present after failure; got %d", len(rows))
	}
	if rows[0].AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1", rows[0].AttemptCount)
	}

	// Second failure compounds.
	if err := MarkPendingFailed(t.Context(), s.DB(), id, 3000, "another"); err != nil {
		t.Fatalf("MarkPendingFailed (2): %v", err)
	}
	rows, _ = PendingBatch(t.Context(), s.DB(), 10)
	if rows[0].AttemptCount != 2 {
		t.Errorf("after 2nd failure attempt_count: got %d, want 2", rows[0].AttemptCount)
	}
}

func TestMarkPendingFailed_TruncatesLongError(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	id, _ := enqueueOne(t, s, "evt", []byte("body"), 1000)

	long := bytes.Repeat([]byte("x"), 2048)
	if err := MarkPendingFailed(t.Context(), s.DB(), id, 2000, string(long)); err != nil {
		t.Fatalf("MarkPendingFailed: %v", err)
	}
	var stored string
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT last_error FROM ingest_pending WHERE id = ?`, id,
	).Scan(&stored); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(stored) != 512 {
		t.Errorf("expected truncation to 512, got %d", len(stored))
	}
}

func TestCountPending_TracksBacklog(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	for i := range 3 {
		enqueueOne(t, s, "evt-"+string(rune('a'+i)), []byte("body"), int64(i+1)*1000)
	}
	n, err := CountPending(t.Context(), s.DB())
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3, got %d", n)
	}

	// Process one — count drops by one.
	rows, _ := PendingBatch(t.Context(), s.DB(), 1)
	_ = WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		return MarkPendingProcessed(t.Context(), tx, rows[0].ID)
	})
	n, _ = CountPending(t.Context(), s.DB())
	if n != 2 {
		t.Errorf("after processing one expected 2, got %d", n)
	}
}

func TestEnqueueAndProcessLifecycle(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Enqueue 3, process 1 — confirms that PendingBatch sees the
	// remaining two and they're the right ones.
	enqueueOne(t, s, "a", []byte("body-a"), 1000)
	enqueueOne(t, s, "b", []byte("body-b"), 2000)
	enqueueOne(t, s, "c", []byte("body-c"), 3000)

	first, err := PendingBatch(t.Context(), s.DB(), 1)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	if first[0].EventID != "a" {
		t.Fatalf("FIFO violation; got %q first", first[0].EventID)
	}
	if err := WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		return MarkPendingProcessed(t.Context(), tx, first[0].ID)
	}); err != nil {
		t.Fatalf("process: %v", err)
	}

	remaining, _ := PendingBatch(t.Context(), s.DB(), 10)
	if len(remaining) != 2 || remaining[0].EventID != "b" || remaining[1].EventID != "c" {
		t.Errorf("remaining backlog wrong; got %+v", remaining)
	}
}
