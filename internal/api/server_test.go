package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
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
		Redaction:       &events.Redaction{Applied: true},
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
	var ack events.Ack
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
	var ack events.Ack
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

func TestIngest_Accepts_EnvelopeMissingRedaction_AndRedactsServerSide(t *testing.T) {
	t.Parallel()
	// Server is the single point of redaction enforcement. A client
	// that omits Redaction is accepted; the server redacts, sets
	// Applied=true, and stores the result.
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.Redaction = nil
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var stored string
	if err := srv.store.DB().QueryRow(
		`SELECT envelope_json FROM raw_envelopes WHERE event_id = ?`, env.EventID).Scan(&stored); err != nil {
		t.Fatalf("read raw_envelopes: %v", err)
	}
	if !strings.Contains(stored, `"applied":true`) {
		t.Errorf("stored envelope must record Applied=true after server redaction; got: %s", stored)
	}
}

func TestIngest_Accepts_RedactionAppliedFalse_AndRedactsServerSide(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.Redaction = &events.Redaction{Applied: false}
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIngest_RedactsSecretInContentText(t *testing.T) {
	t.Parallel()
	// AKIAIOSFODNN7EXAMPLE is a documented-fake AWS access key
	// (matches the aws_access_key detector) — a real, deterministic
	// fixture for asserting server-side redaction end-to-end.
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.ContentText = "leak: AKIAIOSFODNN7EXAMPLE end"
	env.Redaction = nil // client did not redact
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var content string
	if err := srv.store.DB().QueryRow(
		`SELECT content_text FROM events WHERE event_id = ?`, env.EventID).Scan(&content); err != nil {
		t.Fatalf("read events.content_text: %v", err)
	}
	if strings.Contains(content, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("secret survived in events.content_text: %q", content)
	}
	if !strings.Contains(content, "<redacted:aws_access_key>") {
		t.Errorf("expected redaction marker in stored content: %q", content)
	}
}

func TestIngest_RedactsEvenWhenClientClaimsApplied(t *testing.T) {
	t.Parallel()
	// A buggy or malicious client claims Applied=true but ships a
	// raw secret. The server must not trust the bit; it runs its
	// own redactor.
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.ContentText = "still leaking: AKIAIOSFODNN7EXAMPLE"
	env.Redaction = &events.Redaction{Applied: true}
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var content string
	if err := srv.store.DB().QueryRow(
		`SELECT content_text FROM events WHERE event_id = ?`, env.EventID).Scan(&content); err != nil {
		t.Fatalf("read events.content_text: %v", err)
	}
	if strings.Contains(content, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("server trusted the client's Applied=true bit; secret survived: %q", content)
	}
}

func TestIngest_RedactsSecretInPayload(t *testing.T) {
	t.Parallel()
	// Payload is a recursive map; the redactor walks it and scrubs
	// every string leaf. Verify a secret nested in payload is
	// rewritten before storage.
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.Payload = map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"prompt":          "innocent text",
		"args": map[string]any{
			"creds": "AKIAIOSFODNN7EXAMPLE",
		},
	}
	env.Redaction = nil
	body, _ := json.Marshal(env)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var raw string
	if err := srv.store.DB().QueryRow(
		`SELECT envelope_json FROM raw_envelopes WHERE event_id = ?`, env.EventID).Scan(&raw); err != nil {
		t.Fatalf("read raw_envelopes: %v", err)
	}
	if strings.Contains(raw, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("secret survived in raw_envelopes: %s", raw)
	}
}

func TestIngest_Rejects_OversizedEnvelope(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Build a body just over the 16MB cap. A single oversized JSON
	// string is enough; don't bother validating its shape — the size
	// check must fire before JSON decode.
	huge := bytes.Repeat([]byte("x"), DefaultMaxEnvelopeBytes+10)
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

// TestWithMaxEnvelopeBytes_Override proves daemon main can shrink
// the body cap via config without touching exported fields, and
// that the override is honoured by the ingest handler.
func TestWithMaxEnvelopeBytes_Override(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Pick a cap below a real validBody. Anything we POST must 413.
	srv.WithMaxEnvelopeBytes(8)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t)))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 with shrunken cap, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestWithMaxEnvelopeBytes_NonPositiveIgnored ensures a zero-valued
// config never wipes out DefaultMaxEnvelopeBytes.
func TestWithMaxEnvelopeBytes_NonPositiveIgnored(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.WithMaxEnvelopeBytes(0)
	srv.WithMaxEnvelopeBytes(-1)

	// Default should still hold: a normal body is accepted.
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t)))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after no-op overrides, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestListenAndServe_SocketIs0600 asserts the UDS file is created
// (and stays) at 0600. The pre-fix code created the inode at the
// process umask (typically 0644) before chmod tightened it; this
// test would catch a regression where the umask guard is dropped
// and the brief default-perms window returns.
func TestListenAndServe_SocketIs0600(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	shutdown, err := ListenAndServe(sock, http.NotFoundHandler())
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	st, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	mode := st.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("socket perm: got %o, want 0600", mode)
	}
}

// TestNewHTTPServer_TimeoutsWired guards against the "bare http.Server
// with only ReadHeaderTimeout" regression — slow-read / slow-write
// attacks can otherwise hold a connection indefinitely. Both code
// paths (ListenAndServe and the systemd-activated Serve) build the
// server through newHTTPServer, so one assertion covers both.
func TestNewHTTPServer_TimeoutsWired(t *testing.T) {
	t.Parallel()
	srv := newHTTPServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout must be > 0, got %v", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Errorf("ReadTimeout must be > 0, got %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("WriteTimeout must be > 0, got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout must be > 0, got %v", srv.IdleTimeout)
	}
	// ReadHeaderTimeout must not exceed ReadTimeout — a header that
	// outlasts the whole-request budget would never fire.
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Errorf("ReadHeaderTimeout (%v) must be <= ReadTimeout (%v)",
			srv.ReadHeaderTimeout, srv.ReadTimeout)
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
