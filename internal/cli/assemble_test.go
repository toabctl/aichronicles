package cli

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAssemble_UserPromptSubmit(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id": "sess-1",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/tmp/project",
		"prompt": "hello"
	}`)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	env, err := Assemble(raw, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if env.V != 1 {
		t.Errorf("V: got %d, want 1", env.V)
	}
	if _, err := uuid.Parse(env.EventID); err != nil {
		t.Errorf("EventID not a UUID: %v", err)
	}
	if env.SourceAgent != "claude-code" {
		t.Errorf("SourceAgent: got %q", env.SourceAgent)
	}
	if env.SourceSessionID != "sess-1" {
		t.Errorf("SourceSessionID: got %q", env.SourceSessionID)
	}
	if env.Kind != "user_prompt" {
		t.Errorf("Kind: got %q", env.Kind)
	}
	if env.Role != "user" {
		t.Errorf("Role: got %q", env.Role)
	}
	if !env.TsSource.Equal(now) {
		t.Errorf("TsSource: got %v, want %v", env.TsSource, now)
	}
	if env.Cwd != "/tmp/project" {
		t.Errorf("Cwd: got %q", env.Cwd)
	}
	if env.Transport != "hook" {
		t.Errorf("Transport: got %q", env.Transport)
	}
	if env.ContentText != "hello" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
	if env.Payload["session_id"] != "sess-1" {
		t.Errorf("payload not preserved verbatim")
	}
	if err := env.Validate(); err != nil {
		t.Errorf("assembled envelope failed validation: %v", err)
	}
}

func TestAssemble_PostToolUsePopulatesTool(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id": "sess-1",
		"hook_event_name": "PostToolUse",
		"cwd": "/tmp",
		"tool_name": "Bash",
		"tool_input": {"command":"ls"}
	}`)
	env, err := Assemble(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if env.Kind != "tool_use" {
		t.Errorf("Kind: got %q", env.Kind)
	}
	if env.Role != "tool" {
		t.Errorf("Role: got %q", env.Role)
	}
	if env.Tool == nil {
		t.Fatalf("expected Tool populated")
	}
	if env.Tool.Name != "Bash" || env.Tool.NameRaw != "Bash" {
		t.Errorf("Tool name: got %+v", env.Tool)
	}
	if env.ContentText != "Bash" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestAssemble_UnknownHookEvent(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"s","hook_event_name":"NotYetSupported","cwd":"/"}`)
	env, err := Assemble(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if env.Kind != "unknown" {
		t.Errorf("Kind: got %q, want unknown", env.Kind)
	}
	if env.Role != "" {
		t.Errorf("Role should be empty for unknown kind, got %q", env.Role)
	}
	if env.Payload["hook_event_name"] != "NotYetSupported" {
		t.Errorf("raw hook name lost from payload")
	}
}

func TestAssemble_MissingSessionID_IsError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"UserPromptSubmit","cwd":"/"}`)
	_, err := Assemble(raw, time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for missing session_id")
	}
}

func TestAssemble_MalformedJSON_IsError(t *testing.T) {
	t.Parallel()
	_, err := Assemble([]byte("{not json"), time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestRoleForKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"user_prompt":         "user",
		"assistant_message":   "assistant",
		"tool_use":            "tool",
		"tool_result":         "tool",
		"tool_failure":        "tool",
		"session_start":       "system",
		"compact_end":         "system",
		"cwd_changed":         "system",
		"instructions_loaded": "system",
		"unknown":             "",
	}
	for kind, wantRole := range cases {
		if got := roleForKind(kind); got != wantRole {
			t.Errorf("roleForKind(%q) = %q, want %q", kind, got, wantRole)
		}
	}
}

func TestHookKindMap_CoversDocumentedHooks(t *testing.T) {
	t.Parallel()
	// These are the hook names aichronicles setup claude-code installs.
	installedHooks := []string{
		"UserPromptSubmit",
		"Stop",
		"SessionStart",
		"SessionEnd",
		"PostToolUse",
		"PostToolUseFailure",
	}
	for _, name := range installedHooks {
		if _, ok := hookKindMap[name]; !ok {
			t.Errorf("installed hook %q has no kind mapping", name)
		}
	}
}
