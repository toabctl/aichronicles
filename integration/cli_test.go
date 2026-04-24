//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/ingest"
)

func TestCLI_IngestRoundTrip(t *testing.T) {
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
	defer shutdown()

	hook := []byte(`{
		"session_id": "e2e-1",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/tmp/e2e",
		"prompt": "what is the time"
	}`)

	var stderr bytes.Buffer
	if err := cli.RunIngest(bytes.NewReader(hook), &stderr, sock); err != nil {
		t.Fatalf("RunIngest returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}

	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	var got ingest.Envelope
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}

	if got.SourceAgent != "claude-code" {
		t.Errorf("SourceAgent: got %q", got.SourceAgent)
	}
	if got.SourceSessionID != "e2e-1" {
		t.Errorf("SourceSessionID: got %q", got.SourceSessionID)
	}
	if got.Kind != "user_prompt" {
		t.Errorf("Kind: got %q", got.Kind)
	}
	if got.Cwd != "/tmp/e2e" {
		t.Errorf("Cwd: got %q", got.Cwd)
	}
	if got.ContentText != "what is the time" {
		t.Errorf("ContentText: got %q", got.ContentText)
	}
	if got.Transport != "hook" {
		t.Errorf("Transport: got %q", got.Transport)
	}
	want := ingest.DeriveSessionID("claude-code", "e2e-1")
	if got.SessionID != want {
		t.Errorf("SessionID: got %q, want %q", got.SessionID, want)
	}
}

func TestCLI_IngestUnreachableDaemon_DoesNotFail(t *testing.T) {
	// Unreachable socket path; CLI must NEVER return an error to the hook.
	hook := []byte(`{"session_id":"x","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)
	var stderr bytes.Buffer

	err := cli.RunIngest(bytes.NewReader(hook), &stderr, filepath.Join(t.TempDir(), "nope.sock"))
	if err != nil {
		t.Fatalf("expected nil error even when daemon unreachable, got %v", err)
	}
	if !strings.Contains(stderr.String(), "aichronicles ingest") {
		t.Errorf("expected stderr to mention ingest error, got %q", stderr.String())
	}
}
