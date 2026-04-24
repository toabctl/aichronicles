//go:build integration

package integration

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/store"
)

// isolateEnv scopes XDG + notification env so RunIngest never reads
// the user's real config or fires real DBus notifications from a test.
func isolateEnv(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AICHRONICLES_DISABLE_NOTIFY", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "runtime"))
}

func TestCLI_IngestRoundTrip(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	srv := daemon.NewServer(s, nil)
	shutdown, err := daemon.ListenAndServe(sock, srv.Handler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = shutdown() }()

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

	// The event should now be in the store.
	wantSessionID := ingest.DeriveSessionID("claude-code", "e2e-1")
	var kind, cwd, content string
	err = s.DB().QueryRow(
		`SELECT kind, cwd, content_text FROM events WHERE session_id=?`, wantSessionID,
	).Scan(&kind, &cwd, &content)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if kind != "user_prompt" {
		t.Errorf("kind: got %q", kind)
	}
	if cwd != "/tmp/e2e" {
		t.Errorf("cwd: got %q", cwd)
	}
	if content != "what is the time" {
		t.Errorf("content: got %q", content)
	}

	// FTS should find it too.
	var ftsHits int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "time",
	).Scan(&ftsHits)
	if ftsHits != 1 {
		t.Errorf("FTS: got %d, want 1", ftsHits)
	}
}

func TestCLI_IngestRedactsSecretsEndToEnd(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	srv := daemon.NewServer(s, nil)
	shutdown, err := daemon.ListenAndServe(sock, srv.Handler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = shutdown() }()

	// User accidentally pasted an Anthropic API key into a prompt.
	secret := "sk-ant-" + strings.Repeat("a", 40)
	hook := []byte(`{
		"session_id": "e2e-secret",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/tmp/e2e",
		"prompt": "my key is ` + secret + ` do not log"
	}`)

	var stderr bytes.Buffer
	if err := cli.RunIngest(bytes.NewReader(hook), &stderr, sock); err != nil {
		t.Fatalf("RunIngest: %v", err)
	}

	wantSessionID := ingest.DeriveSessionID("claude-code", "e2e-secret")
	var content string
	err = s.DB().QueryRow(
		`SELECT content_text FROM events WHERE session_id=?`, wantSessionID,
	).Scan(&content)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if strings.Contains(content, "sk-ant-") {
		t.Fatalf("stored content still contains raw secret: %q", content)
	}
	if !strings.Contains(content, "<redacted:anthropic_api_key>") {
		t.Errorf("expected redacted marker: %q", content)
	}

	// The raw envelope must also have been scrubbed before it hit the
	// daemon — otherwise a rogue admin with DB access could recover the
	// secret from raw_envelopes.json_bytes.
	var raw string
	err = s.DB().QueryRow(
		`SELECT envelope_json FROM raw_envelopes WHERE event_id IN
			(SELECT event_id FROM events WHERE session_id=?)`, wantSessionID,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("query raw_envelopes: %v", err)
	}
	if strings.Contains(raw, "sk-ant-") {
		t.Fatalf("raw envelope still contains secret: %q", raw)
	}
	if !strings.Contains(raw, `"applied":true`) {
		t.Errorf("expected redaction.applied=true in raw envelope: %q", raw)
	}
}

func TestCLI_IngestUnreachableDaemon_DoesNotFail(t *testing.T) {
	isolateEnv(t)
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
