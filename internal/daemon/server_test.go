package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/store"
)

// newTestServer returns a Server backed by a fresh temp SQLite store.
// The store closes automatically when the test ends.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewServer(s, nil)
}

func validEnvelope(t *testing.T) ingest.Envelope {
	t.Helper()
	return ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-abc",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "hi"},
		Redaction:       &ingest.Redaction{Applied: true},
	}
}

func validBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(validEnvelope(t))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestIngest_Accepts_ValidEnvelope(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t)))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
	}
	var ack ingest.Ack
	if err := json.Unmarshal(rr.Body.Bytes(), &ack); err != nil {
		t.Fatalf("ack decode: %v", err)
	}
	if ack.EventID == "" || ack.SessionID == "" {
		t.Fatalf("expected populated ack, got %+v", ack)
	}
	if ack.Deduped {
		t.Fatalf("expected Deduped=false for first write")
	}

	var n int
	_ = srv.store.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 1 {
		t.Errorf("events table: got %d, want 1", n)
	}
}

func TestIngest_DuplicateReturnsDedupedAck(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := validBody(t)

	// First write: accepted, Deduped=false.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("first: status %d", rr.Code)
	}
	// Second write with identical body (same event_id): deduped.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("second: status %d, body=%s", rr.Code, rr.Body.String())
	}
	var ack ingest.Ack
	_ = json.Unmarshal(rr.Body.Bytes(), &ack)
	if !ack.Deduped {
		t.Errorf("expected Deduped=true on replay, got %+v", ack)
	}

	var rawCount int
	_ = srv.store.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&rawCount)
	if rawCount != 1 {
		t.Errorf("raw_envelopes: got %d, want 1", rawCount)
	}
}

func TestIngest_Rejects_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader("{not json"))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected problem+json, got %q", ct)
	}
}

func TestIngest_Rejects_InvalidEnvelope(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	body := []byte(`{"event_id":"00000000-0000-0000-0000-000000000000","source_agent":"x","source_session_id":"s","kind":"k","ts_source":"2026-01-01T00:00:00Z","payload":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIngest_Rejects_UnknownEnvelopeFields(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	good := validBody(t)
	body := bytes.Join([][]byte{good[:len(good)-1], []byte(`,"extra":"nope"}`)}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIngest_Rejects_EnvelopeMissingRedaction(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.Redaction = nil // simulate a client that skipped redaction
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Redaction required") {
		t.Errorf("expected 'Redaction required' in body: %s", rr.Body.String())
	}

	// Nothing must have been persisted.
	var n int
	_ = srv.store.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&n)
	if n != 0 {
		t.Errorf("raw_envelopes should be empty, got %d", n)
	}
}

func TestIngest_Rejects_RedactionAppliedFalse(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.Redaction = &ingest.Redaction{Applied: false} // claim present but negative
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIngest_Rejects_OversizedEnvelope(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Build a body just over the 16MB cap. A single oversized JSON
	// string is enough; don't bother validating its shape — the size
	// check must fire before JSON decode.
	huge := bytes.Repeat([]byte("x"), maxEnvelopeBytes+10)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(huge))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rr.Code)
	}
}

// TestListenAndServe_ShutdownDrainsInflightRequest proves that a POST
// already being handled at shutdown time runs to completion when a
// drain context is supplied. Without the graceful path this test would
// see a 500 (tx rollback) or a connection reset.
func TestListenAndServe_ShutdownDrainsInflightRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	srvInstance := NewServer(s, nil)

	// Wrap the real handler to gate its return until the test says so.
	// This lets us start shutdown while the request is still in flight.
	releaseHandler := make(chan struct{})
	handlerEntered := make(chan struct{})
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-releaseHandler
		srvInstance.Handler().ServeHTTP(w, r)
	})

	shutdown, err := ListenAndServe(sock, gated)
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 5 * time.Second,
	}

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Post("http://unix/v1/ingest",
			"application/json", bytes.NewReader(validBody(t)))
		if err != nil {
			done <- result{err: err}
			return
		}
		_ = resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()

	// Wait for the handler to start before triggering shutdown.
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		close(releaseHandler)
		t.Fatal("handler never entered")
	}

	// Kick off shutdown in the background with a 5s drain budget.
	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- shutdown(ctx)
	}()

	// Release the handler so the in-flight request finishes.
	close(releaseHandler)

	// The request must complete successfully — drain waited for it.
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request failed during graceful shutdown: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("status during drain: got %d, want 200", r.status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish before drain timeout")
	}

	// And shutdown must now return cleanly (no deadline exceeded).
	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}
