//go:build integration

package integration

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// TestCLI_CodexHookSessionRoundTrip drives a whole Codex session —
// the five hook payloads a real `codex` run fires, in order and
// captured verbatim from codex-cli 0.149.1 — through the actual
// hook → UDS → worker → SQLite path.
//
// Unit tests cover the translator in isolation; this covers the
// seam it has to survive: that a Codex payload validates as an
// envelope (source_agent slug, kind, role), lands under a session
// id derived from "codex-cli", and reaches FTS.
func TestCLI_CodexHookSessionRoundTrip(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.OpenMigrate(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitDrain := startAPIDaemon(t, s, sock)

	const sessionID = "01a060a9-2017-7ec1-9ffc-19951ea1cdac"
	payloads := []string{
		`{"session_id":"` + sessionID + `","cwd":"/tmp/cx/work",
		  "hook_event_name":"SessionStart","model":"gpt-5.6-sol",
		  "permission_mode":"bypassPermissions","source":"startup",
		  "transcript_path":"/tmp/cx/sessions/2026/09/02/rollout-x.jsonl"}`,
		`{"session_id":"` + sessionID + `","turn_id":"t1","cwd":"/tmp/cx/work",
		  "hook_event_name":"UserPromptSubmit","model":"gpt-5.6-sol",
		  "permission_mode":"bypassPermissions",
		  "prompt":"Run the shell command 'cat note.txt'"}`,
		`{"session_id":"` + sessionID + `","turn_id":"t1","cwd":"/tmp/cx/work",
		  "hook_event_name":"PostToolUse","model":"gpt-5.6-sol",
		  "permission_mode":"bypassPermissions","tool_name":"Bash",
		  "tool_input":{"command":"cat note.txt"},
		  "tool_response":"hello world\n",
		  "tool_use_id":"exec-51398ca3-cd31-40bd-9877-d1fb9fbd687f"}`,
		`{"session_id":"` + sessionID + `","turn_id":"t1","cwd":"/tmp/cx/work",
		  "hook_event_name":"Stop","model":"gpt-5.6-sol",
		  "permission_mode":"bypassPermissions","stop_hook_active":false,
		  "last_assistant_message":"hello world"}`,
		`{"session_id":"` + sessionID + `","cwd":"/tmp/cx/work",
		  "hook_event_name":"SessionEnd","reason":"other",
		  "transcript_path":"/tmp/cx/sessions/2026/09/02/rollout-x.jsonl"}`,
	}
	for i, p := range payloads {
		var stderr bytes.Buffer
		if err := cli.RunHook(t.Context(), bytes.NewReader([]byte(p)), &stderr, sock, "codex-cli"); err != nil {
			t.Fatalf("RunHook[%d] returned error: %v", i, err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("RunHook[%d] unexpected stderr: %q", i, stderr.String())
		}
	}
	waitDrain()

	wantSessionID := events.DeriveSessionID("codex-cli", sessionID)

	// content_text is nullable — session_start / session_end carry
	// no human-readable text, so COALESCE rather than a *string.
	rows, err := s.DB().Query(
		`SELECT kind, source_agent, COALESCE(cwd, ''), COALESCE(content_text, '')
		   FROM events WHERE session_id=? ORDER BY ts_source_ms, rowid`, wantSessionID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type row struct{ kind, agent, cwd, content string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.agent, &r.cwd, &r.content); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []row{
		{"session_start", "codex-cli", "/tmp/cx/work", ""},
		{"user_prompt", "codex-cli", "/tmp/cx/work", "Run the shell command 'cat note.txt'"},
		{"tool_use", "codex-cli", "/tmp/cx/work", "Bash cat note.txt"},
		{"assistant_message", "codex-cli", "/tmp/cx/work", "hello world"},
		{"session_end", "codex-cli", "/tmp/cx/work", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("event count: got %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d]:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}

	// The prompt text must be reachable through FTS — the whole
	// point of capturing a Codex session is finding it later.
	var hits int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, `"note.txt"`,
	).Scan(&hits); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits == 0 {
		t.Error("FTS found no rows matching note.txt")
	}
}

// TestCLI_CodexToolFailureStaysToolUse pins the anti-fabrication
// decision end-to-end: Codex reports a failed shell command as an
// ordinary tool_response string with no error channel, so the row
// that lands must be tool_use with the output intact — never a
// guessed tool_failure.
func TestCLI_CodexToolFailureStaysToolUse(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.OpenMigrate(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitDrain := startAPIDaemon(t, s, sock)

	payload := `{"session_id":"cx-fail","cwd":"/tmp/cx/work",
		"hook_event_name":"PostToolUse","tool_name":"Bash",
		"tool_input":{"command":"cat definitely-missing.txt"},
		"tool_response":"cat: definitely-missing.txt: No such file or directory\n"}`

	var stderr bytes.Buffer
	if err := cli.RunHook(t.Context(), bytes.NewReader([]byte(payload)), &stderr, sock, "codex-cli"); err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	waitDrain()

	var kind string
	if err := s.DB().QueryRow(
		`SELECT kind FROM events WHERE session_id=?`,
		events.DeriveSessionID("codex-cli", "cx-fail"),
	).Scan(&kind); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if kind != "tool_use" {
		t.Errorf("kind: got %q, want tool_use (Codex exposes no failure signal to infer from)", kind)
	}
}
