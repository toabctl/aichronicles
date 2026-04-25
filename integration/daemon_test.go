//go:build integration

// Package integration exercises the daemon end-to-end over a real Unix
// socket. Runs only with `go test -tags=integration ./...`.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// spinDaemon wires up a fresh store + UDS listener and returns the
// shutdown closure plus the store for inspection. The shutdown takes
// a context so integration tests can exercise the daemon's drain
// semantics. Callers that don't care can pass context.Background().
func spinDaemon(t *testing.T) (sock string, st *store.Store, shutdown func(context.Context) error) {
	t.Helper()
	dir := t.TempDir()
	sock = filepath.Join(dir, "sock")

	s, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	srv := daemon.NewServer(s, nil)
	sh, err := daemon.ListenAndServe(sock, srv.Handler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return sock, s, sh
}

func udsClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 2 * time.Second,
	}
}

func TestDaemon_RoundTrip(t *testing.T) {
	sock, st, shutdown := spinDaemon(t)
	defer func() { _ = shutdown(context.Background()) }()

	env := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "roundtrip-session",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Cwd:             "/tmp/fake",
		Transport:       "hook",
		ContentText:     "hello daemon",
		Payload: map[string]any{
			"hook_event_name": "UserPromptSubmit",
			"prompt":          "hello daemon",
		},
		Redaction: &ingest.Redaction{Applied: true},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := udsClient(sock).Post("http://unix/v1/ingest", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var ack ingest.Ack
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatalf("ack decode: %v", err)
	}
	if ack.EventID != env.EventID {
		t.Fatalf("ack event_id: got %q want %q", ack.EventID, env.EventID)
	}
	wantSessionID := ingest.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	if ack.SessionID != wantSessionID {
		t.Fatalf("ack session_id: got %q want %q", ack.SessionID, wantSessionID)
	}

	// Verify the DB layers all populated.
	var rawJSON string
	if err := st.DB().QueryRow(
		`SELECT envelope_json FROM raw_envelopes WHERE event_id=?`, env.EventID,
	).Scan(&rawJSON); err != nil {
		t.Fatalf("raw row: %v", err)
	}
	if rawJSON != string(body) {
		t.Errorf("raw envelope_json not verbatim")
	}

	var cnt int
	_ = st.DB().QueryRow(`SELECT event_count FROM sessions WHERE id=?`, wantSessionID).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("session event_count: got %d, want 1", cnt)
	}

	var fts int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "hello").Scan(&fts)
	if fts != 1 {
		t.Errorf("FTS match count: got %d, want 1", fts)
	}
}

func TestDaemon_DuplicateIsDeduped(t *testing.T) {
	sock, _, shutdown := spinDaemon(t)
	defer func() { _ = shutdown(context.Background()) }()

	env := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "dup-session",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		ContentText:     "dup",
		Payload:         map[string]any{},
		Redaction:       &ingest.Redaction{Applied: true},
	}
	body, _ := json.Marshal(env)

	client := udsClient(sock)
	for i, wantDeduped := range []bool{false, true} {
		resp, err := client.Post("http://unix/v1/ingest", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		var ack ingest.Ack
		_ = json.NewDecoder(resp.Body).Decode(&ack)
		_ = resp.Body.Close()
		if ack.Deduped != wantDeduped {
			t.Errorf("POST %d: Deduped got %v, want %v", i, ack.Deduped, wantDeduped)
		}
	}
}

func TestDaemon_Healthz(t *testing.T) {
	sock, _, shutdown := spinDaemon(t)
	defer func() { _ = shutdown(context.Background()) }()

	resp, err := udsClient(sock).Get("http://unix/v1/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
