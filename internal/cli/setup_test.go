package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/agents"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestInstall_CreatesFreshSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	report, err := InstallClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(report, "added 6 hook entries") {
		t.Errorf("report should list all 6 added: %q", report)
	}

	got := readJSON(t, path)
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks missing from settings")
	}
	for _, name := range agents.ClaudeCode.HookEvents {
		entries, ok := hooks[name].([]any)
		if !ok || len(entries) != 1 {
			t.Errorf("%s: expected 1 entry, got %v", name, hooks[name])
		}
	}
}

func TestInstall_PreservesUnknownTopLevelFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	initial := map[string]any{
		"model":             "claude-opus-4-7",
		"autoMemoryEnabled": true,
		"customField":       map[string]any{"nested": "value"},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	got := readJSON(t, path)
	if got["model"] != "claude-opus-4-7" {
		t.Errorf("lost top-level model: %v", got["model"])
	}
	if got["autoMemoryEnabled"] != true {
		t.Errorf("lost autoMemoryEnabled")
	}
	if _, ok := got["customField"]; !ok {
		t.Errorf("lost customField")
	}
	if _, ok := got["hooks"]; !ok {
		t.Errorf("hooks not added")
	}
}

func TestInstall_KeepsExistingHooksFromOtherTools(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	initial := map[string]any{
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
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	got := readJSON(t, path)
	entries := got["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (other-tool + ours), got %d: %v", len(entries), entries)
	}

	// First entry preserved unchanged
	first := entries[0].(map[string]any)
	firstInner := first["hooks"].([]any)[0].(map[string]any)
	if firstInner["command"] != "other-tool" {
		t.Errorf("other-tool entry mutated: %v", firstInner)
	}

	// Second entry is ours
	second := entries[1].(map[string]any)
	secondInner := second["hooks"].([]any)[0].(map[string]any)
	if secondInner["command"] != defaultHookCommand {
		t.Errorf("our entry wrong: %v", secondInner)
	}
}

func TestInstall_IsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	firstContent, _ := os.ReadFile(path)

	report, err := InstallClaudeCodeHooks(path, "")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !strings.Contains(report, "no changes") {
		t.Errorf("second run should report no changes, got %q", report)
	}
	secondContent, _ := os.ReadFile(path)
	if string(firstContent) != string(secondContent) {
		t.Errorf("second run mutated the file")
	}
}

func TestInstall_RespectsCustomCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := InstallClaudeCodeHooks(path, "/opt/bin/aichronicles ingest --socket /var/run/my.sock"); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := readJSON(t, path)
	entries := got["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	inner := entries[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if inner["command"] != "/opt/bin/aichronicles ingest --socket /var/run/my.sock" {
		t.Errorf("custom command not written: %v", inner["command"])
	}
}

func TestInstall_WritesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := InstallClaudeCodeHooks(path, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	// No temp files left behind
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "settings.json" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}

	// File perms locked down
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("perm: got %o, want 0600", got)
	}
}

func TestInstall_MalformedJSON_IsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := InstallClaudeCodeHooks(path, ""); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestEntryHasCommand(t *testing.T) {
	t.Parallel()
	entries := []any{
		map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": "other"},
				map[string]any{"type": "command", "command": "aichronicles ingest"},
			},
		},
	}
	if !entryHasCommand(entries, "aichronicles ingest") {
		t.Errorf("expected match for existing command")
	}
	if entryHasCommand(entries, "missing") {
		t.Errorf("false match for absent command")
	}
}
