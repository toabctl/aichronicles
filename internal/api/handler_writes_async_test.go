package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// newAsyncTestServer is a thin wrapper around the same shape
// newTestServer uses; the dedicated helper survives because
// these tests assert behaviour specific to the async pipeline
// (queue full → 503, pending-row staging) that's awkward to
// distinguish from the catch-all ingest tests in server_test.go.
// NewServer constructs the worker unconditionally; tests drive
// Worker().drain() directly rather than starting Run.
func newAsyncTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := store.OpenMigrate(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewServer(s, nil)
}

func TestIngestAsync_PostEnqueuesAndDoesNotWriteSyncRows(t *testing.T) {
	t.Parallel()
	srv := newAsyncTestServer(t)
	body := validBody(t)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body)))

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
	}
	var ack events.Ack
	if err := json.Unmarshal(rr.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.EventID == "" || ack.SessionID == "" {
		t.Fatalf("ack must be populated, got %+v", ack)
	}
	if ack.Deduped {
		t.Errorf("first POST should not be deduped")
	}

	// The headline async invariant: the request returned 200, but
	// the redact + extract + downstream write have NOT happened
	// yet. Pending row should exist; raw_envelopes / events should
	// still be empty.
	pending, err := store.CountPending(t.Context(), srv.store.DB())
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if pending != 1 {
		t.Errorf("ingest_pending: got %d, want 1", pending)
	}
	var rawCount int
	_ = srv.store.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&rawCount)
	if rawCount != 0 {
		t.Errorf("raw_envelopes should still be empty before worker drains; got %d", rawCount)
	}
}

func TestIngestAsync_WorkerDrainCommitsToRawEnvelopes(t *testing.T) {
	t.Parallel()
	srv := newAsyncTestServer(t)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t))))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d", rr.Code)
	}
	var ack events.Ack
	_ = json.Unmarshal(rr.Body.Bytes(), &ack)

	// Drive the worker.
	if err := srv.Worker().drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Pending row gone, raw_envelopes populated with our event.
	pending, _ := store.CountPending(t.Context(), srv.store.DB())
	if pending != 0 {
		t.Errorf("ingest_pending should be empty after drain; got %d", pending)
	}
	var n int
	_ = srv.store.DB().QueryRow(
		`SELECT COUNT(*) FROM raw_envelopes WHERE event_id = ?`, ack.EventID).Scan(&n)
	if n != 1 {
		t.Errorf("event_id %s missing from raw_envelopes (count=%d)", ack.EventID, n)
	}
}

func TestIngestAsync_DuplicateEnvelopeReturnsDedupedAtPhaseOne(t *testing.T) {
	t.Parallel()
	srv := newAsyncTestServer(t)
	body := validBody(t)

	// First POST: accepted, not deduped.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("first POST status: %d", rr.Code)
	}
	var first events.Ack
	_ = json.Unmarshal(rr.Body.Bytes(), &first)
	if first.Deduped {
		t.Error("first POST should not be deduped")
	}

	// Second POST with same event_id BEFORE the worker drains —
	// must be deduped at the ingest_pending UNIQUE constraint.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("second POST status: %d", rr.Code)
	}
	var second events.Ack
	_ = json.Unmarshal(rr.Body.Bytes(), &second)
	if !second.Deduped {
		t.Error("second POST with same event_id should be deduped at phase 1")
	}
	if second.EventID != first.EventID {
		t.Errorf("ack mismatch on dedup: first=%q second=%q", first.EventID, second.EventID)
	}

	// Only one pending row.
	pending, _ := store.CountPending(t.Context(), srv.store.DB())
	if pending != 1 {
		t.Errorf("ingest_pending: got %d, want 1", pending)
	}
}

func TestIngestAsync_QueueFullReturns503(t *testing.T) {
	t.Parallel()
	srv := newAsyncTestServer(t)
	// Tighten the cap so the test doesn't have to enqueue 10000
	// rows to hit it.
	srv.ingestQueueMax = 2

	// Two fills to capacity.
	for i := range 2 {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t))))
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %d: expected 200, got %d", i, rr.Code)
		}
	}

	// Third one: queue full → 503.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t))))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 at full queue; got %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("expected problem+json, got %q", ct)
	}
}

func TestIngestAsync_InvalidEnvelopeRejectedBeforeEnqueue(t *testing.T) {
	t.Parallel()
	srv := newAsyncTestServer(t)

	// Malformed JSON: must still 400, not enqueue.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader([]byte("{not"))))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: got %d, want 400", rr.Code)
	}
	pending, _ := store.CountPending(t.Context(), srv.store.DB())
	if pending != 0 {
		t.Errorf("malformed envelope leaked into ingest_pending; pending=%d", pending)
	}
}
