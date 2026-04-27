package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeMCPServer_AddsNewEntry(t *testing.T) {
	t.Parallel()
	settings := map[string]any{}
	added := mergeMCPServer(settings, "aichronicles", "aichronicles", []string{"mcp-serve"})
	if !added {
		t.Fatal("merge into empty settings should add")
	}
	mcp, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers root missing")
	}
	entry, ok := mcp["aichronicles"].(map[string]any)
	if !ok {
		t.Fatal("entry missing")
	}
	if entry["command"] != "aichronicles" {
		t.Errorf("command: got %v", entry["command"])
	}
	args, _ := entry["args"].([]any)
	if len(args) != 1 || args[0] != "mcp-serve" {
		t.Errorf("args: got %v", entry["args"])
	}
}

func TestMergeMCPServer_NoOpWhenSameEntryAlreadyPresent(t *testing.T) {
	t.Parallel()
	settings := map[string]any{}
	mergeMCPServer(settings, "aichronicles", "aichronicles", []string{"mcp-serve"})
	again := mergeMCPServer(settings, "aichronicles", "aichronicles", []string{"mcp-serve"})
	if again {
		t.Error("re-merging an identical entry should be a no-op")
	}
}

// TestMergeMCPServer_PreservesUserEdit covers the conservative
// rule: if the entry exists with a DIFFERENT command, we don't
// overwrite. Treating a hand-edited config as user intent.
func TestMergeMCPServer_PreservesUserEdit(t *testing.T) {
	t.Parallel()
	settings := map[string]any{
		"mcpServers": map[string]any{
			"aichronicles": map[string]any{
				"command": "/custom/path/aichronicles",
				"args":    []any{"mcp-serve", "--db", "/tmp/x"},
			},
		},
	}
	added := mergeMCPServer(settings, "aichronicles", "aichronicles", []string{"mcp-serve"})
	if added {
		t.Error("should not overwrite a user-edited entry")
	}
	entry := settings["mcpServers"].(map[string]any)["aichronicles"].(map[string]any)
	if entry["command"] != "/custom/path/aichronicles" {
		t.Errorf("user edit got clobbered: %v", entry["command"])
	}
}

// TestMergeMCPServer_PreservesOtherServers ensures merging our
// entry doesn't touch other MCP servers the user has registered.
func TestMergeMCPServer_PreservesOtherServers(t *testing.T) {
	t.Parallel()
	settings := map[string]any{
		"mcpServers": map[string]any{
			"linear": map[string]any{"command": "linear-mcp"},
		},
	}
	mergeMCPServer(settings, "aichronicles", "aichronicles", []string{"mcp-serve"})
	mcp := settings["mcpServers"].(map[string]any)
	if _, ok := mcp["linear"]; !ok {
		t.Error("linear entry got clobbered")
	}
	if _, ok := mcp["aichronicles"]; !ok {
		t.Error("aichronicles entry not added alongside")
	}
}

func TestStripMCPServer_DeletesNamedEntry(t *testing.T) {
	t.Parallel()
	settings := map[string]any{
		"mcpServers": map[string]any{
			"aichronicles": map[string]any{"command": "aichronicles"},
			"linear":       map[string]any{"command": "linear-mcp"},
		},
	}
	if !stripMCPServer(settings, "aichronicles") {
		t.Fatal("strip should report mutation")
	}
	mcp := settings["mcpServers"].(map[string]any)
	if _, gone := mcp["aichronicles"]; gone {
		t.Error("aichronicles still present")
	}
	if _, kept := mcp["linear"]; !kept {
		t.Error("linear entry should survive")
	}
}

func TestStripMCPServer_DeletesEmptyMapAfterRemoval(t *testing.T) {
	t.Parallel()
	settings := map[string]any{
		"mcpServers": map[string]any{
			"aichronicles": map[string]any{"command": "aichronicles"},
		},
	}
	stripMCPServer(settings, "aichronicles")
	if _, ok := settings["mcpServers"]; ok {
		t.Error("empty mcpServers map should be deleted")
	}
}

func TestStripMCPServer_NoOpWhenAbsent(t *testing.T) {
	t.Parallel()
	settings := map[string]any{}
	if stripMCPServer(settings, "aichronicles") {
		t.Error("strip on absent entry should report no mutation")
	}
}

// TestInstallClaudeCodeFull_WritesHooksAndMCP is the load-bearing
// integration test: hooks land in settingsPath (~/.claude/settings.json)
// AND the MCP server registers in userConfigPath (~/.claude.json) —
// two different files because that's how Claude Code splits them.
func TestInstallClaudeCodeFull_WritesHooksAndMCP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	userPath := filepath.Join(dir, "user.json")

	report, err := InstallClaudeCodeFull(settingsPath, userPath, defaultHookCommand,
		&MCPServerEntry{Name: "aichronicles", Command: "aichronicles"})
	if err != nil {
		t.Fatalf("InstallClaudeCodeFull: %v", err)
	}
	for _, want := range []string{
		"added 6 hook entries",
		"registered mcpServers.aichronicles",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n%s", want, report)
		}
	}

	// Hooks land in the settings file; mcpServers does NOT.
	settingsBody, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(settingsBody, &settings); err != nil {
		t.Fatalf("parse settings: %v\n%s", err, settingsBody)
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing from settings.json")
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop", "PostToolUse",
		"PostToolUseFailure", "SessionStart", "SessionEnd"} {
		if _, ok := hooks[ev]; !ok {
			t.Errorf("hook event %s missing", ev)
		}
	}
	if _, leaked := settings["mcpServers"]; leaked {
		t.Errorf("mcpServers should NOT be in settings.json (belongs in user-config)")
	}

	// MCP servers land in the user-config file; hooks do NOT.
	userBody, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("read user config: %v", err)
	}
	var user map[string]any
	if err := json.Unmarshal(userBody, &user); err != nil {
		t.Fatalf("parse user config: %v\n%s", err, userBody)
	}
	mcp, ok := user["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers block missing from ~/.claude.json")
	}
	entry := mcp["aichronicles"].(map[string]any)
	if entry["command"] != "aichronicles" {
		t.Errorf("MCP command: got %v", entry["command"])
	}
	if _, leaked := user["hooks"]; leaked {
		t.Errorf("hooks should NOT be in ~/.claude.json")
	}
}

// TestInstallClaudeCodeFull_NilMCPSkipsRegistration covers the
// --skip-mcp path: hooks land but the user-config file is left
// untouched (not even created).
func TestInstallClaudeCodeFull_NilMCPSkipsRegistration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	userPath := filepath.Join(dir, "user.json")

	if _, err := InstallClaudeCodeFull(settingsPath, userPath, defaultHookCommand, nil); err != nil {
		t.Fatalf("InstallClaudeCodeFull: %v", err)
	}
	body, _ := os.ReadFile(settingsPath)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if _, ok := parsed["hooks"]; !ok {
		t.Errorf("hooks should land in settings.json:\n%s", body)
	}
	if _, err := os.Stat(userPath); !os.IsNotExist(err) {
		t.Errorf("--skip-mcp should NOT create the user-config file: %v", err)
	}
}

// TestRemoveClaudeCodeFull_StripsBothHooksAndMCP verifies the
// install/teardown pair round-trips cleanly across both files:
// after teardown each file looks identical to a never-installed
// state, and unrelated entries (sibling settings, foreign MCP
// servers) survive.
func TestRemoveClaudeCodeFull_StripsBothHooksAndMCP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	userPath := filepath.Join(dir, "user.json")
	// Seed hooks settings.json with an unrelated sibling key.
	settingsSeed, _ := json.MarshalIndent(map[string]any{
		"security": map[string]any{"sshKeyEnabled": true},
	}, "", "  ")
	if err := os.WriteFile(settingsPath, settingsSeed, 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	// Seed user-config with a foreign MCP server we'll check survives.
	userSeed, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"linear": map[string]any{"command": "linear-mcp"},
		},
	}, "", "  ")
	if err := os.WriteFile(userPath, userSeed, 0o644); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if _, err := InstallClaudeCodeFull(settingsPath, userPath, defaultHookCommand,
		&MCPServerEntry{Name: "aichronicles", Command: "aichronicles"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	report, err := RemoveClaudeCodeFull(settingsPath, userPath, defaultHookCommand, false)
	if err != nil {
		t.Fatalf("RemoveClaudeCodeFull: %v", err)
	}
	if !strings.Contains(report, "removed") {
		t.Errorf("report missing 'removed':\n%s", report)
	}

	settingsBody, _ := os.ReadFile(settingsPath)
	var settings map[string]any
	_ = json.Unmarshal(settingsBody, &settings)
	if _, ok := settings["hooks"]; ok {
		t.Errorf("hooks block should be empty / removed:\n%s", settingsBody)
	}
	if _, kept := settings["security"]; !kept {
		t.Errorf("security block should survive teardown")
	}

	userBody, _ := os.ReadFile(userPath)
	var user map[string]any
	_ = json.Unmarshal(userBody, &user)
	mcp, ok := user["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers should still exist (linear is left)")
	}
	if _, gone := mcp["aichronicles"]; gone {
		t.Errorf("aichronicles should be gone after teardown")
	}
	if _, kept := mcp["linear"]; !kept {
		t.Errorf("linear should survive teardown")
	}
}

// TestRemoveClaudeCodeFull_DryRunPreviewsWithoutMutation pins the
// dry-run guarantee: neither file is modified, but the report says
// what would happen.
func TestRemoveClaudeCodeFull_DryRunPreviewsWithoutMutation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	userPath := filepath.Join(dir, "user.json")
	if _, err := InstallClaudeCodeFull(settingsPath, userPath, defaultHookCommand,
		&MCPServerEntry{Name: "aichronicles", Command: "aichronicles"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	beforeSettings, _ := os.ReadFile(settingsPath)
	beforeUser, _ := os.ReadFile(userPath)

	report, err := RemoveClaudeCodeFull(settingsPath, userPath, defaultHookCommand, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(report, "would remove") {
		t.Errorf("dry-run report missing 'would remove':\n%s", report)
	}
	afterSettings, _ := os.ReadFile(settingsPath)
	afterUser, _ := os.ReadFile(userPath)
	if string(beforeSettings) != string(afterSettings) {
		t.Errorf("dry-run modified settings:\nbefore=%s\nafter=%s", beforeSettings, afterSettings)
	}
	if string(beforeUser) != string(afterUser) {
		t.Errorf("dry-run modified user config:\nbefore=%s\nafter=%s", beforeUser, afterUser)
	}
}

func TestRemoveClaudeCodeFull_NothingInstalledIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	userPath := filepath.Join(dir, "user.json")
	report, err := RemoveClaudeCodeFull(settingsPath, userPath, defaultHookCommand, false)
	if err != nil {
		t.Fatalf("RemoveClaudeCodeFull: %v", err)
	}
	if !strings.Contains(report, "nothing to remove") {
		t.Errorf("report should say 'nothing to remove':\n%s", report)
	}
}
