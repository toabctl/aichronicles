package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
)

// startUDSTestServer runs handler on a temp UDS and returns the socket
// path plus a teardown function. Lets us exercise the UDS Client without
// pulling in the daemon package.
func startUDSTestServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	return sock, func() {
		_ = srv.Close()
		_ = os.Remove(sock)
	}
}

func validEnvelope(t *testing.T) ingest.Envelope {
	t.Helper()
	return ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "test-session",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"k": "v"},
	}
}

func TestClient_Post_Success(t *testing.T) {
	t.Parallel()

	var gotPath, gotCT string
	var gotEnv ingest.Envelope
	sock, teardown := startUDSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotEnv)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ingest.Ack{
			EventID:   gotEnv.EventID,
			SessionID: "derived-session",
			Deduped:   false,
		})
	}))
	defer teardown()

	c := NewClient(sock)
	env := validEnvelope(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ack, err := c.Post(ctx, env)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotPath != "/v1/ingest" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type: got %q", gotCT)
	}
	if gotEnv.EventID != env.EventID {
		t.Errorf("server saw event_id %q, want %q", gotEnv.EventID, env.EventID)
	}
	if ack.SessionID != "derived-session" {
		t.Errorf("ack session_id: got %q", ack.SessionID)
	}
}

func TestClient_Post_NonOKReturnsError(t *testing.T) {
	t.Parallel()

	sock, teardown := startUDSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"title":"nope","status":400}`))
	}))
	defer teardown()

	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Post(ctx, validEnvelope(t))
	if err == nil {
		t.Fatal("expected error on 400")
	}
}

func TestClient_Post_UnreachableSocketReturnsError(t *testing.T) {
	t.Parallel()
	c := NewClient(filepath.Join(t.TempDir(), "does-not-exist.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Post(ctx, validEnvelope(t))
	if err == nil {
		t.Fatal("expected error for unreachable socket")
	}
}

// Path-resolution tests live in internal/paths; this file only
// exercises the UDS Client.
