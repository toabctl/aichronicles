package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/agents"
)

// writeJSON seeds a settings/hooks file with the given contents.
func writeJSON(t *testing.T, path string, v map[string]any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// TestInstallCodexHooks_WritesCodexShapedFile checks the whole file
// Codex will read, not just that our entries landed. Codex's
// hooks.json uses the same `hooks.<Event>[].hooks[]` container as
// Claude's settings.json — that equivalence is the reason
// InstallAgentHooks can be shared, so it's worth pinning.
func TestInstallCodexHooks_WritesCodexShapedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")

	report, err := InstallAgentHooks(agents.CodexCLI, path, defaultHookCommandFor(agents.CodexCLI))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(report, "added 5 hook entries") {
		t.Errorf("report should list all 5 added: %q", report)
	}

	got := readJSON(t, path)
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks missing from %s: %v", path, got)
	}
	if len(hooks) != len(agents.CodexCLI.HookEvents) {
		t.Errorf("hooks: got %d events, want %d", len(hooks), len(agents.CodexCLI.HookEvents))
	}
	for _, name := range agents.CodexCLI.HookEvents {
		entries, ok := hooks[name].([]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("%s: expected 1 entry, got %v", name, hooks[name])
		}
		entry, ok := entries[0].(map[string]any)
		if !ok {
			t.Fatalf("%s: entry is not an object: %v", name, entries[0])
		}
		inner, ok := entry["hooks"].([]any)
		if !ok || len(inner) != 1 {
			t.Fatalf("%s: expected 1 inner hook, got %v", name, entry["hooks"])
		}
		h, _ := inner[0].(map[string]any)
		if h["type"] != "command" {
			t.Errorf("%s: type: got %v, want command", name, h["type"])
		}
		if want := "aichronicles hook --agent codex-cli"; h["command"] != want {
			t.Errorf("%s: command: got %v, want %q", name, h["command"], want)
		}
	}
}

// TestInstallCodexHooks_PreservesOtherToolsEntries: Codex's
// hooks.json is a shared file — anything already registered there
// (and trusted) must survive our merge, or we silently disable
// somebody else's hook.
func TestInstallCodexHooks_PreservesOtherToolsEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	writeJSON(t, path, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": "bash /home/u/.codex/other.sh", "timeout": 10},
				}},
			},
		},
	})

	if _, err := InstallAgentHooks(agents.CodexCLI, path, defaultHookCommandFor(agents.CodexCLI)); err != nil {
		t.Fatalf("install: %v", err)
	}

	hooks := readJSON(t, path)["hooks"].(map[string]any)
	entries, _ := hooks["SessionStart"].([]any)
	if len(entries) != 2 {
		t.Fatalf("SessionStart: got %d entries, want 2 (theirs + ours)", len(entries))
	}
	first, _ := entries[0].(map[string]any)
	inner, _ := first["hooks"].([]any)
	h, _ := inner[0].(map[string]any)
	if h["command"] != "bash /home/u/.codex/other.sh" {
		t.Errorf("pre-existing hook was clobbered: %v", h)
	}
	if h["timeout"] != float64(10) {
		t.Errorf("pre-existing hook lost its timeout: %v", h)
	}
}

func TestInstallCodexHooks_IsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	cmd := defaultHookCommandFor(agents.CodexCLI)

	if _, err := InstallAgentHooks(agents.CodexCLI, path, cmd); err != nil {
		t.Fatalf("first install: %v", err)
	}
	report, err := InstallAgentHooks(agents.CodexCLI, path, cmd)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !strings.Contains(report, "no changes") {
		t.Errorf("second install should be a no-op: %q", report)
	}
}

// TestRemoveCodexHooks_RoundTrips: install then teardown must leave
// the file as pristine as it started.
func TestRemoveCodexHooks_RoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	cmd := defaultHookCommandFor(agents.CodexCLI)

	if _, err := InstallAgentHooks(agents.CodexCLI, path, cmd); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Dry run first: reports the plan, changes nothing.
	report, err := RemoveAgentHooks(agents.CodexCLI, path, cmd, true)
	if err != nil {
		t.Fatalf("dry-run remove: %v", err)
	}
	if !strings.Contains(report, "would remove") {
		t.Errorf("dry-run report should use would-remove phrasing: %q", report)
	}
	if hooks, ok := readJSON(t, path)["hooks"].(map[string]any); !ok || len(hooks) == 0 {
		t.Errorf("dry run must not modify the file, got %v", readJSON(t, path))
	}

	if _, err := RemoveAgentHooks(agents.CodexCLI, path, cmd, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got := readJSON(t, path)
	if _, present := got["hooks"]; present {
		t.Errorf("empty hooks container should be cleaned up, got %v", got)
	}
}

// TestCodexHookCommandCarriesAgentFlag pins the one thing that must
// match between install and dispatch: a hook registered without
// --agent codex-cli would be translated as claude-code and label
// every Codex row with the wrong agent.
func TestCodexHookCommandCarriesAgentFlag(t *testing.T) {
	t.Parallel()
	got := defaultHookCommandFor(agents.CodexCLI)
	if want := "aichronicles hook --agent codex-cli"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestTranslateHook_DispatchesByAgentSlug covers the other half of
// the install contract: the slug baked into the hook command must
// route to a translator that stamps the matching source_agent. A
// silent fall-through to claude-code here would mislabel every row
// while looking perfectly healthy.
func TestTranslateHook_DispatchesByAgentSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		slug string
		raw  string
		want string
	}{
		{"claude-code", `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"hi"}`, "claude-code"},
		{"gemini-cli", `{"session_id":"s","hook_event_name":"BeforeAgent","prompt":"hi"}`, "gemini-cli"},
		{"codex-cli", `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"hi"}`, "codex-cli"},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			t.Parallel()
			env, err := translateHook(tc.slug, []byte(tc.raw))
			if err != nil {
				t.Fatalf("translateHook: %v", err)
			}
			if env.SourceAgent != tc.want {
				t.Errorf("SourceAgent: got %q, want %q", env.SourceAgent, tc.want)
			}
			if env.ContentText != "hi" {
				t.Errorf("ContentText: got %q, want hi", env.ContentText)
			}
		})
	}
}

func TestTranslateHook_UnknownSlugErrors(t *testing.T) {
	t.Parallel()
	if _, err := translateHook("codex", []byte(`{"session_id":"s"}`)); err == nil {
		t.Error("expected an error for an unregistered slug")
	}
}

// TestInstallCodexHooks_SessionEndCarriesExplicitTimeout: Codex
// defaults SessionEnd hooks to a 1s timeout (every other event gets
// 600s) and caps it at 3s. We ask for the cap explicitly, because
// the ingest budget this project ships has historically overrun 1s
// on a loaded box.
func TestInstallCodexHooks_SessionEndCarriesExplicitTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	if _, err := InstallAgentHooks(agents.CodexCLI, path, defaultHookCommandFor(agents.CodexCLI)); err != nil {
		t.Fatalf("install: %v", err)
	}

	hooks := readJSON(t, path)["hooks"].(map[string]any)
	timeoutFor := func(event string) (any, bool) {
		t.Helper()
		entries, _ := hooks[event].([]any)
		if len(entries) != 1 {
			t.Fatalf("%s: expected 1 entry, got %v", event, hooks[event])
		}
		entry, _ := entries[0].(map[string]any)
		inner, _ := entry["hooks"].([]any)
		h, _ := inner[0].(map[string]any)
		v, present := h["timeout"]
		return v, present
	}

	got, present := timeoutFor("SessionEnd")
	if !present {
		t.Error("SessionEnd: no timeout written; Codex would apply its 1s default")
	} else if got != float64(3) {
		t.Errorf("SessionEnd timeout: got %v, want 3", got)
	}

	// Every other event must stay silent on timeout — writing one
	// would replace Codex's generous 600s default with our guess.
	for _, ev := range agents.CodexCLI.HookEvents {
		if ev == "SessionEnd" {
			continue
		}
		if v, present := timeoutFor(ev); present {
			t.Errorf("%s: unexpected timeout %v; should inherit the host default", ev, v)
		}
	}
}

// TestInstallHooks_AgentsWithoutTimeoutsWriteNoTimeoutKey guards the
// shared merge path: adding the Codex timeout must not start
// stamping a `timeout` onto Claude Code's or Gemini's entries.
func TestInstallHooks_AgentsWithoutTimeoutsWriteNoTimeoutKey(t *testing.T) {
	t.Parallel()
	for _, agent := range []agents.Agent{agents.ClaudeCode, agents.GeminiCLI} {
		t.Run(agent.Slug, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "settings.json")
			if _, err := InstallAgentHooks(agent, path, defaultHookCommandFor(agent)); err != nil {
				t.Fatalf("install: %v", err)
			}
			hooks := readJSON(t, path)["hooks"].(map[string]any)
			for _, ev := range agent.HookEvents {
				entries, _ := hooks[ev].([]any)
				entry, _ := entries[0].(map[string]any)
				inner, _ := entry["hooks"].([]any)
				h, _ := inner[0].(map[string]any)
				if v, present := h["timeout"]; present {
					t.Errorf("%s/%s: unexpected timeout %v", agent.Slug, ev, v)
				}
			}
		})
	}
}

// TestRemoveCodexHooks_StripsTimedEntries: teardown matches on the
// command string, so an entry carrying a timeout must still be
// recognised as ours and removed.
func TestRemoveCodexHooks_StripsTimedEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.json")
	cmd := defaultHookCommandFor(agents.CodexCLI)
	if _, err := InstallAgentHooks(agents.CodexCLI, path, cmd); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := RemoveAgentHooks(agents.CodexCLI, path, cmd, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, present := readJSON(t, path)["hooks"]; present {
		t.Errorf("SessionEnd's timed entry survived teardown: %v", readJSON(t, path))
	}
}
