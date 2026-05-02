package claude

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/pkg/events"
)

// Fixed timestamp for deterministic test output. Real hooks fire
// with the wall clock; tests inject this.
var testNow = func() time.Time { return time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC) }

func TestHookTranslator_UserPromptShape(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id": "sess-abc",
		"hook_event_name": "UserPromptSubmit",
		"prompt": "what changed in main.go?",
		"cwd": "/tmp/proj"
	}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.SourceAgent != "claude-code" {
		t.Errorf("SourceAgent: got %q, want claude-code", env.SourceAgent)
	}
	if env.Kind != events.KindUserPrompt {
		t.Errorf("Kind: got %q, want %q", env.Kind, events.KindUserPrompt)
	}
	if env.Role != events.RoleUser {
		t.Errorf("Role: got %q, want %q", env.Role, events.RoleUser)
	}
	if env.ContentText != "what changed in main.go?" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
	if env.Cwd != "/tmp/proj" {
		t.Errorf("Cwd: got %q", env.Cwd)
	}
	if env.Transport != "hook" {
		t.Errorf("Transport: got %q", env.Transport)
	}
	// Translator is now pure — Redaction is left for the receiving
	// daemon's Pipeline to populate. Assert it's not pre-set so a
	// future regression that re-adds edge redaction is caught.
	if env.Redaction != nil {
		t.Errorf("Redaction must be nil from a pure translator; got %+v", env.Redaction)
	}
}

func TestHookTranslator_ToolUseRendersContent(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id": "sess-abc",
		"hook_event_name": "PostToolUse",
		"tool_name": "Bash",
		"tool_input": {"command": "ls -la", "description": "list files"}
	}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Kind != events.KindToolUse {
		t.Errorf("Kind: got %q, want %q", env.Kind, events.KindToolUse)
	}
	if env.Tool == nil || env.Tool.Name != "Bash" {
		t.Errorf("Tool: got %+v", env.Tool)
	}
	if env.ContentText != "Bash ls -la" {
		t.Errorf("ContentText: got %q, want \"Bash ls -la\"", env.ContentText)
	}
}

func TestHookTranslator_MissingSessionIDErrors(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name": "UserPromptSubmit"}`)
	tr := &HookTranslator{Now: testNow}
	_, err := tr.Translate(raw)
	if err == nil {
		t.Fatalf("expected error for missing session_id")
	}
}

func TestHookTranslator_UnknownHookEventBecomesKindUnknown(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id": "x", "hook_event_name": "FrobNitz"}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Kind != events.KindUnknown {
		t.Errorf("Kind: got %q, want %q", env.Kind, events.KindUnknown)
	}
}

func TestHookTranslator_SubagentExtractedWhenAgentIDPresent(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id": "x",
		"hook_event_name": "SubagentStart",
		"agent_id": "sub-7",
		"agent_type": "planner"
	}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Subagent == nil || env.Subagent.ID != "sub-7" || env.Subagent.Type != "planner" {
		t.Errorf("Subagent: got %+v", env.Subagent)
	}
}

func TestHookTranslator_EventIDIsValidUUID(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id": "x", "hook_event_name": "UserPromptSubmit", "prompt": "hi"}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// Validate enforces UUID shape on EventID; if Validate passes
	// the EventID is a valid UUID, which is what we want.
	if err := env.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestHookTranslator_FullPayloadRoundTrips confirms the Payload
// field carries the verbatim hook input. Every field that arrived
// in the JSON must be present in env.Payload — anything we drop
// here is a real loss.
func TestHookTranslator_FullPayloadRoundTrips(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id": "sess-1",
		"hook_event_name": "PostToolUse",
		"tool_name": "Bash",
		"tool_input": {"command": "echo hi"},
		"cwd": "/p",
		"some_unknown_field": "preserved"
	}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// Re-marshal the Payload and compare key sets — exact byte
	// equality with raw is impossible (map key order is
	// non-deterministic), so check field presence.
	keys := []string{"session_id", "hook_event_name", "tool_name", "tool_input", "cwd", "some_unknown_field"}
	for _, k := range keys {
		if _, ok := env.Payload[k]; !ok {
			t.Errorf("payload missing key %q", k)
		}
	}
	// Round-trip the payload through JSON to confirm it's
	// serializable (the daemon expects this).
	if _, err := json.Marshal(env.Payload); err != nil {
		t.Errorf("payload not JSON-marshalable: %v", err)
	}
}
