//go:build integration

// Package integration exercises the daemon end-to-end over a real Unix
// socket. Runs only with `go test -tags=integration ./...`.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/ingest"
)

func TestDaemon_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	logPath := filepath.Join(dir, "events.jsonl")

	lg, err := daemon.OpenLogger(logPath)
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}
	defer lg.Close()

	srv := daemon.NewServer(lg, nil)
	shutdown, err := daemon.ListenAndServe(sock, srv.Handler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		if err := shutdown(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 2 * time.Second,
	}

	env := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "roundtrip-session",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Cwd:             "/tmp/fake",
		Transport:       "hook",
		Payload: map[string]any{
			"hook_event_name": "UserPromptSubmit",
			"prompt":          "hello daemon",
		},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := client.Post("http://unix/v1/ingest", "application/json", bytes.NewReader(body))
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
		t.Fatalf("ack event_id mismatch: got %q want %q", ack.EventID, env.EventID)
	}
	wantSessionID := ingest.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	if ack.SessionID != wantSessionID {
		t.Fatalf("ack session_id mismatch: got %q want %q", ack.SessionID, wantSessionID)
	}

	// Give the file a moment to flush; AppendJSON writes synchronously
	// but the OS layer can stagger - re-open to read.
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line in log, got %d", len(lines))
	}

	var got ingest.Envelope
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("log line unmarshal: %v", err)
	}
	if got.EventID != env.EventID {
		t.Fatalf("log event_id mismatch: got %q want %q", got.EventID, env.EventID)
	}
	if got.SessionID != wantSessionID {
		t.Fatalf("log session_id not derived: got %q want %q", got.SessionID, wantSessionID)
	}
	if got.TsServer.IsZero() {
		t.Fatalf("expected ts_server populated on persist")
	}
}

func TestDaemon_Healthz(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	lg, err := daemon.OpenLogger(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}
	defer lg.Close()

	srv := daemon.NewServer(lg, nil)
	shutdown, err := daemon.ListenAndServe(sock, srv.Handler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer shutdown()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://unix/v1/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
