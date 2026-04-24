package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemove_NoSettingsFile_NoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	report, err := RemoveClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(report, "no aichronicles hooks found") {
		t.Errorf("expected no-op report, got %q", report)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file not to be created, stat err=%v", err)
	}
}

func TestRemove_AfterInstall_CleansUpCompletely(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Install fresh, then immediately remove.
	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	report, err := RemoveClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(report, "removed 6 hook entries") {
		t.Errorf("report: %q", report)
	}

	got := readJSON(t, path)
	if _, exists := got["hooks"]; exists {
		t.Errorf("expected hooks section to be removed entirely, got: %v", got["hooks"])
	}
}

func TestRemove_PreservesOtherToolsHooks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Seed: an existing "other-tool" entry on UserPromptSubmit.
	seed := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "other-tool"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Install ours alongside, then teardown.
	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := RemoveClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got := readJSON(t, path)
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks section should still exist (other-tool still installed)")
	}
	entries := hooks["UserPromptSubmit"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 remaining entry (other-tool), got %d: %v", len(entries), entries)
	}
	inner := entries[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if inner["command"] != "other-tool" {
		t.Errorf("other-tool entry mutated: %v", inner)
	}

	// Events only aichronicles touched should be cleaned up entirely.
	for _, ev := range []string{"Stop", "PostToolUse", "SessionStart", "SessionEnd", "PostToolUseFailure"} {
		if _, exists := hooks[ev]; exists {
			t.Errorf("expected %s to be removed (only contained ours), got %v", ev, hooks[ev])
		}
	}
}

func TestRemove_IsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := RemoveClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	afterFirst, _ := os.ReadFile(path)

	report, err := RemoveClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if !strings.Contains(report, "no aichronicles hooks found") {
		t.Errorf("second run should be no-op, got %q", report)
	}
	afterSecond, _ := os.ReadFile(path)
	if string(afterFirst) != string(afterSecond) {
		t.Errorf("second remove mutated the file")
	}
}

func TestRemove_PreservesUnknownTopLevelFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Sprinkle unrelated top-level state after install.
	got := readJSON(t, path)
	got["model"] = "claude-haiku-4-5"
	got["autoMemoryEnabled"] = false
	data, _ := json.MarshalIndent(got, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if _, err := RemoveClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := readJSON(t, path)
	if after["model"] != "claude-haiku-4-5" {
		t.Errorf("lost model: %v", after["model"])
	}
	if after["autoMemoryEnabled"] != false {
		t.Errorf("lost autoMemoryEnabled: %v", after["autoMemoryEnabled"])
	}
}

func TestRemove_KeepsMixedEntryIntact(t *testing.T) {
	t.Parallel()
	// An unusual entry that bundles our command with another tool's in
	// a single entry. Safer to leave it alone than to mutate what
	// looks like someone else's configuration.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	seed := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": defaultHookCommand},
						map[string]any{"type": "command", "command": "co-resident-tool"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	report, err := RemoveClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(report, "no aichronicles hooks found") {
		t.Errorf("mixed entry should NOT be stripped, got %q", report)
	}

	got := readJSON(t, path)
	entries := got["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entry should still be there")
	}
	inner := entries[0].(map[string]any)["hooks"].([]any)
	if len(inner) != 2 {
		t.Errorf("inner hooks should still be 2, got %d", len(inner))
	}
}

func TestRemove_MalformedJSON_IsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := RemoveClaudeCodeHooks(path, ""); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestRemove_CustomCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	custom := "/opt/bin/aichronicles ingest --socket /var/run/my.sock"

	if _, err := InstallClaudeCodeHooks(path, custom); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Removing with the default command should NOT strip the custom install.
	report, err := RemoveClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("remove default: %v", err)
	}
	if !strings.Contains(report, "no aichronicles hooks found") {
		t.Errorf("default teardown should not touch custom command, got %q", report)
	}
	// Removing with the matching command should strip it.
	if _, err := RemoveClaudeCodeHooks(path, custom); err != nil {
		t.Fatalf("remove custom: %v", err)
	}
	got := readJSON(t, path)
	if _, exists := got["hooks"]; exists {
		t.Errorf("expected full cleanup after custom-command teardown, got %v", got["hooks"])
	}
}
