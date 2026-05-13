package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

func validEnvelope(t *testing.T) events.Envelope {
	t.Helper()
	return events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-abc",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "hi"},
	}
}

// newRealServerClient stands up the actual internal/api server
// over httptest (TCP, but the server's wire shape is identical).
// Returns a Client wired to it. Verifies the Ingest method end-
// to-end against the real handler instead of a stub.
//
// The api.Server runs two-phase ingest: the handler enqueues to
// ingest_pending and returns 200 immediately, while a background
// worker drains the table into raw_envelopes / events. Tests
// that assert downstream state must waitForIngestDrain after
// each Ingest call.
func newRealServerClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	apiSrv := api.NewServer(st, nil)
	// Start the ingest worker so the staging table actually
	// drains. Bound to a ctx the test cleans up; the worker
	// exits on cancel via its internal shutdown drain.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	t.Cleanup(cancelWorker)
	go func() { _ = apiSrv.Worker().Run(workerCtx) }()

	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	return newClientForTests(srv.Client(), srv.URL), st
}

// waitForIngestDrain blocks until the worker has emptied
// ingest_pending or the test's hard deadline expires. Polls
// once every 2ms — fine for tests, never used in production.
func waitForIngestDrain(t *testing.T, st *store.Store) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := store.CountPending(t.Context(), st.DB())
		if err != nil {
			t.Fatalf("CountPending: %v", err)
		}
		if n == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("worker did not drain ingest_pending within 2s")
}

func TestClient_Ingest_HappyPath(t *testing.T) {
	t.Parallel()
	c, st := newRealServerClient(t)
	env := validEnvelope(t)

	ack, err := c.Ingest(context.Background(), env)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ack.EventID != env.EventID {
		t.Errorf("Ack.EventID: got %q, want %q", ack.EventID, env.EventID)
	}
	if ack.Deduped {
		t.Errorf("first ingest must not be Deduped")
	}

	// Wait for the worker to commit the staged row downstream.
	waitForIngestDrain(t, st)

	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if n != 1 {
		t.Errorf("events table: got %d, want 1", n)
	}
}

func TestClient_Ingest_DuplicateRetainsSingleRawEnvelope(t *testing.T) {
	t.Parallel()
	c, st := newRealServerClient(t)
	env := validEnvelope(t)

	if _, err := c.Ingest(context.Background(), env); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	// Let the worker commit the first envelope downstream
	// before the second POST — otherwise the second POST
	// races the worker and the ack's Deduped flag could go
	// either way depending on timing. Production hooks don't
	// see this because they don't immediately retry the same
	// event_id at sub-millisecond intervals; tests need the
	// explicit barrier.
	waitForIngestDrain(t, st)

	if _, err := c.Ingest(context.Background(), env); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	waitForIngestDrain(t, st)

	// The permanent dedup signal lives in raw_envelopes
	// (UNIQUE(event_id)): regardless of how either ack's
	// Deduped flag resolved, the row count stays at one.
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&n); err != nil {
		t.Fatalf("query raw_envelopes: %v", err)
	}
	if n != 1 {
		t.Errorf("raw_envelopes: got %d, want 1", n)
	}
}

func TestClient_Ingest_InvalidEnvelope_IsBadRequestError(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)

	// Build an envelope that fails Validate: blank EventID.
	env := validEnvelope(t)
	env.EventID = ""

	_, err := c.Ingest(context.Background(), env)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if herr.Status != http.StatusBadRequest {
		t.Errorf("Status: got %d, want 400", herr.Status)
	}
}

func TestClient_Ingest_DaemonDown_IsErrSocketUnavailable(t *testing.T) {
	t.Parallel()
	// Real UDS path that doesn't exist — exercises the
	// production constructor end-to-end.
	missing := filepath.Join(t.TempDir(), "nope.sock")
	c := NewClient(missing)
	_, err := c.Ingest(context.Background(), validEnvelope(t))
	if !errors.Is(err, ErrSocketUnavailable) {
		t.Errorf("got %v, want ErrSocketUnavailable", err)
	}
}
