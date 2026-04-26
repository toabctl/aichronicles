package cli

import (
	"strings"
	"testing"
	"time"
)

func TestAssembleByAgent_DispatchesToCodex(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"codex-1",
		"hook_event_name":"UserPromptSubmit",
		"cwd":"/tmp/c",
		"prompt":"hello from codex"
	}`)
	env, err := AssembleByAgent("codex", raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleByAgent codex: %v", err)
	}
	if env.SourceAgent != "codex" {
		t.Errorf("SourceAgent: got %q, want codex", env.SourceAgent)
	}
	if env.Kind != "user_prompt" {
		t.Errorf("Kind: got %q, want user_prompt", env.Kind)
	}
	if env.ContentText != "hello from codex" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestAssembleByAgent_ClaudeCodeStaysDefault(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"cc-1",
		"hook_event_name":"UserPromptSubmit",
		"cwd":"/tmp/cc",
		"prompt":"hello from claude code"
	}`)
	env, err := AssembleByAgent("claude-code", raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleByAgent claude-code: %v", err)
	}
	if env.SourceAgent != "claude-code" {
		t.Errorf("SourceAgent: got %q, want claude-code", env.SourceAgent)
	}
}

func TestAssembleByAgent_UnknownSlugIsError(t *testing.T) {
	t.Parallel()
	_, err := AssembleByAgent("not-a-thing", []byte("{}"), time.Now())
	if err == nil {
		t.Fatal("expected error for unknown agent slug")
	}
	if !strings.Contains(err.Error(), "not-a-thing") {
		t.Errorf("error should mention the slug, got %v", err)
	}
}

func TestAssembleCodex_PostToolUseRendersToolDetail(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"c2",
		"hook_event_name":"PostToolUse",
		"cwd":"/work",
		"tool_name":"Bash",
		"tool_input":{"command":"ls -la"}
	}`)
	env, err := AssembleCodex(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleCodex: %v", err)
	}
	if env.Kind != "tool_use" {
		t.Errorf("Kind: got %q, want tool_use", env.Kind)
	}
	if env.Tool == nil || env.Tool.Name != "Bash" {
		t.Errorf("Tool: got %+v", env.Tool)
	}
	if !strings.Contains(env.ContentText, "Bash ls -la") {
		t.Errorf("ContentText: got %q, want substring 'Bash ls -la'", env.ContentText)
	}
}

func TestAssembleCodex_UnknownEventMapsToUnknown(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"c3","hook_event_name":"NotYetSupported","cwd":"/"}`)
	env, err := AssembleCodex(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleCodex: %v", err)
	}
	if env.Kind != "unknown" {
		t.Errorf("Kind: got %q, want unknown", env.Kind)
	}
}

func TestAssembleCodex_ToolFailureSurfacesError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"c-fail",
		"hook_event_name":"PostToolUseFailure",
		"cwd":"/work",
		"tool_name":"Bash",
		"tool_input":{"command":"false"},
		"error":"Exit code 1",
		"exit_code":1,
		"stderr":"some failure detail"
	}`)
	env, err := AssembleCodex(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleCodex: %v", err)
	}
	if env.Kind != "tool_failure" {
		t.Fatalf("Kind: got %q, want tool_failure", env.Kind)
	}
	for _, want := range []string{
		"Bash false", // base rendering from renderToolContent
		"error: Exit code 1",
		"stderr: some failure detail",
		"exit_code: 1",
	} {
		if !strings.Contains(env.ContentText, want) {
			t.Errorf("ContentText missing %q:\n%s", want, env.ContentText)
		}
	}
}

func TestAssembleCodex_ToolFailureWithoutErrorFieldsFallsBack(t *testing.T) {
	t.Parallel()
	// Some failures carry only the bare tool info — make sure
	// renderCodexToolFailure degrades to the same shape
	// renderToolContent would have produced rather than emitting
	// an empty string.
	raw := []byte(`{
		"session_id":"c-bare",
		"hook_event_name":"PostToolUseFailure",
		"cwd":"/work",
		"tool_name":"Bash",
		"tool_input":{"command":"false"}
	}`)
	env, err := AssembleCodex(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleCodex: %v", err)
	}
	if env.ContentText != "Bash false" {
		t.Errorf("ContentText: got %q, want %q", env.ContentText, "Bash false")
	}
}

func TestAssembleCodex_MissingSessionIDIsError(t *testing.T) {
	t.Parallel()
	_, err := AssembleCodex([]byte(`{"hook_event_name":"UserPromptSubmit"}`), time.Now())
	if err == nil {
		t.Fatal("expected error for missing session_id")
	}
}
