package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
)

// newTestWorker wires up a worker against a fresh temp store using
// the same Pipeline shape NewServer constructs in production. The
// SSE bus has no logger — its drop-warn behaviour is exercised in
// sse_bus_test.go; this test focuses on the worker's pending-table
// state machine.
func newTestWorker(t *testing.T) (*IngestWorker, *store.Store, *sseBus) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bus := newSSEBus(nil)
	t.Cleanup(bus.Close)
	pipeline := events.Pipeline{
		Sink:       store.NewSink(s),
		Extractors: events.DefaultExtractors(),
		Redactor:   events.NewScannerRedactor(redact.Default()),
	}
	return NewIngestWorker(s, pipeline, bus, nil), s, bus
}

// validEnvelopeBytes returns a redacted-flag-true envelope JSON
// blob. Worker tests bypass the HTTP handler and write directly to
// ingest_pending, so the test caller is responsible for the same
// validation guarantees the handler would have provided.
func validEnvelopeBytes(t *testing.T, eventID string) []byte {
	t.Helper()
	env := events.Envelope{
		V:               1,
		EventID:         eventID,
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-worker-test",
		Kind:            "user_prompt",
		TsSource:        time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC),
		Cwd:             "/tmp/x",
		ContentText:     "hello from worker test",
		Payload:         map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "hi"},
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

// enqueueDirect inserts a pending row outside the worker so tests
// can stage state without spinning up the HTTP layer.
func enqueueDirect(t *testing.T, s *store.Store, eventID string, body []byte) int64 {
	t.Helper()
	var id int64
	if err := store.WithTx(t.Context(), s.DB(), func(tx *sql.Tx) error {
		var err error
		id, _, err = store.EnqueuePending(t.Context(), tx, eventID, body, time.Now().UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	return id
}

func TestIngestWorker_DrainProcessesPendingRowsAndDeletes(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)

	id := uuid.Must(uuid.NewV7()).String()
	enqueueDirect(t, s, id, validEnvelopeBytes(t, id))

	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// The pending row must be gone.
	n, err := store.CountPending(t.Context(), s.DB())
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 0 {
		t.Errorf("pending backlog: got %d, want 0", n)
	}

	// The event must have landed in raw_envelopes.
	var count int
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM raw_envelopes WHERE event_id = ?`, id,
	).Scan(&count); err != nil {
		t.Fatalf("scan raw_envelopes: %v", err)
	}
	if count != 1 {
		t.Errorf("raw_envelopes row count: got %d, want 1", count)
	}
}

func TestIngestWorker_DrainProcessesMultipleInFIFOOrder(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)

	ids := make([]string, 3)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7()).String()
		enqueueDirect(t, s, ids[i], validEnvelopeBytes(t, ids[i]))
		// Tiny gap so received_at_ms differs deterministically; the
		// worker drains FIFO, so insertion order matters only via
		// the timestamp.
		time.Sleep(2 * time.Millisecond)
	}

	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	n, _ := store.CountPending(t.Context(), s.DB())
	if n != 0 {
		t.Errorf("expected all rows drained, %d remain", n)
	}
	// All three event_ids should be in raw_envelopes.
	for _, id := range ids {
		var c int
		_ = s.DB().QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM raw_envelopes WHERE event_id = ?`, id).Scan(&c)
		if c != 1 {
			t.Errorf("event %s missing from raw_envelopes", id)
		}
	}
}

func TestIngestWorker_DrainPublishesToSSEOnNonDeduped(t *testing.T) {
	t.Parallel()
	w, s, bus := newTestWorker(t)

	ch, cancel, ok := bus.subscribe()
	if !ok {
		t.Fatal("subscribe rejected")
	}
	defer cancel()

	id := uuid.Must(uuid.NewV7()).String()
	enqueueDirect(t, s, id, validEnvelopeBytes(t, id))

	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.EventID != id {
			t.Errorf("SSE event_id: got %q, want %q", ev.EventID, id)
		}
		if ev.Kind != "user_prompt" {
			t.Errorf("SSE kind: got %q, want %q", ev.Kind, "user_prompt")
		}
		// IngestSeq is what the SSE handler renders as `id: N`
		// in the wire frame; a reconnecting subscriber resumes
		// from this via Last-Event-ID. Before the fix in
		// commit-this-lands, every frame carried 0, breaking
		// resume. The first event in a fresh store gets seq=1
		// because the seq table starts at 1 (migration 008's
		// initial INSERT) and UPDATE...RETURNING returns the
		// pre-increment value.
		if ev.IngestSeq != 1 {
			t.Errorf("SSE ingest_seq: got %d, want 1 (first event in fresh store)", ev.IngestSeq)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not publish to SSE bus")
	}
}

func TestIngestWorker_DrainSkipsSSEOnDedupedReprocess(t *testing.T) {
	t.Parallel()
	w, s, bus := newTestWorker(t)

	id := uuid.Must(uuid.NewV7()).String()
	body := validEnvelopeBytes(t, id)

	// First pass: process the row normally — SSE fires.
	enqueueDirect(t, s, id, body)
	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("first drain: %v", err)
	}

	// Stage the SAME event_id in ingest_pending again, simulating
	// the "we crashed between event commit and pending delete"
	// recovery path. The worker should detect the dup via
	// raw_envelopes and skip the SSE publish.
	enqueueDirect(t, s, id, body)

	ch, cancel, _ := bus.subscribe()
	defer cancel()
	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	select {
	case ev, ok := <-ch:
		if ok {
			t.Errorf("expected no SSE event on deduped reprocess; got %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
		// No event arrived — correct.
	}
	// Pending row should still be gone after the dedup-y reprocess.
	n, _ := store.CountPending(t.Context(), s.DB())
	if n != 0 {
		t.Errorf("deduped reprocess left %d pending rows", n)
	}
}

func TestIngestWorker_DrainRecordsFailureOnMalformedBody(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)

	id := enqueueDirect(t, s, "evt-bad", []byte("{not json"))

	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Row must still be there with attempt_count bumped.
	rows, err := store.PendingBatch(t.Context(), s.DB(), 10)
	if err != nil {
		t.Fatalf("PendingBatch: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("expected the bad row to stay; got %+v", rows)
	}
	if rows[0].AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1", rows[0].AttemptCount)
	}
	var lastErr string
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT last_error FROM ingest_pending WHERE id = ?`, id).Scan(&lastErr); err != nil {
		t.Fatalf("scan last_error: %v", err)
	}
	if !strings.Contains(lastErr, "unmarshal") {
		t.Errorf("last_error: got %q, want substring %q", lastErr, "unmarshal")
	}
}

func TestIngestWorker_DrainRecordsFailureOnValidateFailure(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)

	// Valid JSON, invalid envelope (missing required event_id).
	body, _ := json.Marshal(events.Envelope{
		V:           1,
		SourceAgent: "claude-code",
		Kind:        "user_prompt",
		Redaction:   &events.Redaction{Applied: true},
	})
	enqueueDirect(t, s, "evt-incomplete", body)

	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	rows, _ := store.PendingBatch(t.Context(), s.DB(), 10)
	if len(rows) != 1 {
		t.Fatalf("expected the invalid row to stay; got %d rows", len(rows))
	}
	if rows[0].AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1", rows[0].AttemptCount)
	}
}

func TestIngestWorker_DrainContinuesPastFailedRow(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)

	// First row: malformed (will fail). Second row: valid (should
	// still be processed even though the first row blew up).
	enqueueDirect(t, s, "evt-bad", []byte("{nope"))
	time.Sleep(2 * time.Millisecond)
	goodID := uuid.Must(uuid.NewV7()).String()
	enqueueDirect(t, s, goodID, validEnvelopeBytes(t, goodID))

	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Only the bad row should remain.
	rows, _ := store.PendingBatch(t.Context(), s.DB(), 10)
	if len(rows) != 1 || rows[0].EventID != "evt-bad" {
		t.Fatalf("expected only the bad row to remain; got %+v", rows)
	}
	// The good row landed downstream.
	var c int
	_ = s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM raw_envelopes WHERE event_id = ?`, goodID).Scan(&c)
	if c != 1 {
		t.Errorf("good event did not land downstream after sibling failure")
	}
}

func TestIngestWorker_WakeIsNonBlocking(t *testing.T) {
	t.Parallel()
	w, _, _ := newTestWorker(t)
	// Fill the slot — second Wake must not block.
	w.Wake()
	done := make(chan struct{})
	go func() {
		w.Wake()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wake blocked when slot was full")
	}
}

func TestIngestWorker_RunExitsOnContextCancel(t *testing.T) {
	t.Parallel()
	w, _, _ := newTestWorker(t)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = w.Run(ctx) }()

	cancel()
	select {
	case <-w.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}

func TestIngestWorker_RunInitialDrainPicksUpExistingRows(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)

	// Pre-stage a row, THEN start Run. Run's first action is a
	// drain pass — covers the daemon-restart scenario where the
	// previous process left work in ingest_pending.
	id := uuid.Must(uuid.NewV7()).String()
	enqueueDirect(t, s, id, validEnvelopeBytes(t, id))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// Poll for drain completion. Initial drain runs synchronously
	// before the select loop, but Run starts it in a goroutine —
	// so we wait briefly for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := store.CountPending(t.Context(), s.DB())
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("initial drain did not clear the backlog")
}

func TestIngestWorker_FailureEscalatesToErrorAtMaxAttempts(t *testing.T) {
	t.Parallel()
	w, s, _ := newTestWorker(t)
	w.maxAttempts = 2 // make the escalation observable in two drains

	// Capture log records so we can assert the level escalation.
	cap := &captureHandler{}
	w.log = slog.New(cap)

	enqueueDirect(t, s, "evt-bad", []byte("{nope"))

	// First drain: Warn level.
	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	// Second drain: escalation to Error.
	if err := w.drain(t.Context()); err != nil {
		t.Fatalf("drain 2: %v", err)
	}

	var sawWarn, sawError bool
	for _, r := range cap.snapshot() {
		if r.Message != "ingest worker: row failed" {
			continue
		}
		switch r.Level {
		case slog.LevelWarn:
			sawWarn = true
		case slog.LevelError:
			sawError = true
		}
	}
	if !sawWarn {
		t.Error("first failure should log at Warn")
	}
	if !sawError {
		t.Error("max-attempt failure should escalate to Error")
	}
}
