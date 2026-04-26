package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

func TestInstallAgentHooks_CodexWritesAllEvents(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")

	cmd := defaultHookCommandFor(ingest.Codex)
	report, err := InstallAgentHooks(ingest.Codex, path, cmd)
	if err != nil {
		t.Fatalf("InstallAgentHooks: %v", err)
	}
	if !strings.Contains(report, "added") {
		t.Errorf("report should mention added events: %s", report)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks section missing in %s", string(raw))
	}
	for _, ev := range ingest.Codex.HookEvents {
		if _, ok := hooks[ev]; !ok {
			t.Errorf("event %q not registered in %s", ev, string(raw))
		}
	}
	// The command body should carry --agent codex so RunIngest
	// picks the right assembler.
	if !strings.Contains(string(raw), "--agent codex") {
		t.Errorf("hook command should include --agent codex: %s", string(raw))
	}
}

func TestInstallAgentHooks_CodexIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	cmd := defaultHookCommandFor(ingest.Codex)
	if _, err := InstallAgentHooks(ingest.Codex, path, cmd); err != nil {
		t.Fatalf("first install: %v", err)
	}
	report, err := InstallAgentHooks(ingest.Codex, path, cmd)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !strings.Contains(report, "no changes") {
		t.Errorf("second install should be a no-op: %s", report)
	}
}

func TestRemoveAgentHooks_CodexStripsOurs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	cmd := defaultHookCommandFor(ingest.Codex)
	if _, err := InstallAgentHooks(ingest.Codex, path, cmd); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Dry-run first.
	report, err := RemoveAgentHooks(ingest.Codex, path, cmd, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(report, "would remove") {
		t.Errorf("dry-run report missing 'would remove': %s", report)
	}

	// Confirm.
	if _, err := RemoveAgentHooks(ingest.Codex, path, cmd, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "aichronicles ingest") {
		t.Errorf("hooks.json should be clean after teardown: %s", string(raw))
	}
}

func TestRemoveAgentHooks_CodexPreservesOtherTools(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	// Plant a hooks.json with a non-aichronicles entry alongside
	// ours. Teardown must leave the foreign entry untouched.
	const seed = `{
		"hooks": {
			"PostToolUse": [
				{"hooks":[{"type":"command","command":"some-other-tool log"}]},
				{"hooks":[{"type":"command","command":"aichronicles ingest --agent codex"}]}
			]
		}
	}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := RemoveAgentHooks(ingest.Codex, path,
		"aichronicles ingest --agent codex", false); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "some-other-tool log") {
		t.Errorf("foreign tool entry was removed: %s", string(raw))
	}
	if strings.Contains(string(raw), "aichronicles") {
		t.Errorf("our entry was not removed: %s", string(raw))
	}
}

func TestDefaultHookCommandFor_AgentSpecific(t *testing.T) {
	t.Parallel()
	if got := defaultHookCommandFor(ingest.ClaudeCode); got != "aichronicles ingest" {
		t.Errorf("claude-code default: got %q, want bare 'aichronicles ingest'", got)
	}
	if got := defaultHookCommandFor(ingest.Codex); got != "aichronicles ingest --agent codex" {
		t.Errorf("codex default: got %q, want '--agent codex' suffix", got)
	}
}
