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
// integration test: a single call mutates settings.json with both
// hook entries and the MCP server registration in one atomic
// write.
func TestInstallClaudeCodeFull_WritesHooksAndMCP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	report, err := InstallClaudeCodeFull(path, defaultHookCommand,
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

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	hooks, ok := parsed["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop", "PostToolUse",
		"PostToolUseFailure", "SessionStart", "SessionEnd"} {
		if _, ok := hooks[ev]; !ok {
			t.Errorf("hook event %s missing", ev)
		}
	}
	mcp, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers block missing")
	}
	entry := mcp["aichronicles"].(map[string]any)
	if entry["command"] != "aichronicles" {
		t.Errorf("MCP command: got %v", entry["command"])
	}
}

// TestInstallClaudeCodeFull_NilMCPSkipsRegistration covers the
// --skip-mcp path: hooks land but no mcpServers entry is created.
func TestInstallClaudeCodeFull_NilMCPSkipsRegistration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := InstallClaudeCodeFull(path, defaultHookCommand, nil); err != nil {
		t.Fatalf("InstallClaudeCodeFull: %v", err)
	}
	body, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if _, ok := parsed["mcpServers"]; ok {
		t.Errorf("--skip-mcp should NOT write mcpServers:\n%s", body)
	}
	if _, ok := parsed["hooks"]; !ok {
		t.Errorf("hooks should still land:\n%s", body)
	}
}

// TestRemoveClaudeCodeFull_StripsBothHooksAndMCP verifies the
// install/teardown pair round-trips cleanly: after teardown the
// file looks identical to a never-installed state.
func TestRemoveClaudeCodeFull_StripsBothHooksAndMCP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Pre-populate with a sibling key + a foreign MCP server so
	// we can confirm those survive the teardown.
	seed, _ := json.MarshalIndent(map[string]any{
		"security": map[string]any{"sshKeyEnabled": true},
		"mcpServers": map[string]any{
			"linear": map[string]any{"command": "linear-mcp"},
		},
	}, "", "  ")
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := InstallClaudeCodeFull(path, defaultHookCommand,
		&MCPServerEntry{Name: "aichronicles", Command: "aichronicles"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	report, err := RemoveClaudeCodeFull(path, defaultHookCommand, false)
	if err != nil {
		t.Fatalf("RemoveClaudeCodeFull: %v", err)
	}
	if !strings.Contains(report, "removed") {
		t.Errorf("report missing 'removed':\n%s", report)
	}

	body, _ := os.ReadFile(path)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)

	if _, ok := parsed["hooks"]; ok {
		t.Errorf("hooks block should be empty / removed:\n%s", body)
	}
	mcp, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers should still exist (linear is left)")
	}
	if _, gone := mcp["aichronicles"]; gone {
		t.Errorf("aichronicles should be gone after teardown")
	}
	if _, kept := mcp["linear"]; !kept {
		t.Errorf("linear should survive teardown")
	}
	if _, kept := parsed["security"]; !kept {
		t.Errorf("security block should survive teardown")
	}
}

// TestRemoveClaudeCodeFull_DryRunPreviewsWithoutMutation pins the
// dry-run guarantee: the file is unchanged, but the report says
// what would happen.
func TestRemoveClaudeCodeFull_DryRunPreviewsWithoutMutation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if _, err := InstallClaudeCodeFull(path, defaultHookCommand,
		&MCPServerEntry{Name: "aichronicles", Command: "aichronicles"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	before, _ := os.ReadFile(path)

	report, err := RemoveClaudeCodeFull(path, defaultHookCommand, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(report, "would remove") {
		t.Errorf("dry-run report missing 'would remove':\n%s", report)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("dry-run modified the file:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestRemoveClaudeCodeFull_NothingInstalledIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	report, err := RemoveClaudeCodeFull(path, defaultHookCommand, false)
	if err != nil {
		t.Fatalf("RemoveClaudeCodeFull: %v", err)
	}
	if !strings.Contains(report, "nothing to remove") {
		t.Errorf("report should say 'nothing to remove':\n%s", report)
	}
}
