package cli

import (
	"strings"
	"testing"
	"time"
)

func TestAssembleByAgent_DispatchesToGemini(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"g1",
		"hook_event_name":"BeforeAgent",
		"cwd":"/tmp/g",
		"prompt":"hello from gemini"
	}`)
	env, err := AssembleByAgent("gemini-cli", raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleByAgent gemini-cli: %v", err)
	}
	if env.SourceAgent != "gemini-cli" {
		t.Errorf("SourceAgent: got %q, want gemini-cli", env.SourceAgent)
	}
	if env.Kind != "user_prompt" {
		t.Errorf("Kind: got %q, want user_prompt", env.Kind)
	}
	if env.ContentText != "hello from gemini" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestAssembleGemini_BeforeAgentIsUserPrompt(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"g-bp",
		"hook_event_name":"BeforeAgent",
		"cwd":"/work",
		"prompt":"investigate the slow query"
	}`)
	env, err := AssembleGemini(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleGemini: %v", err)
	}
	if env.Kind != "user_prompt" {
		t.Errorf("Kind: got %q, want user_prompt", env.Kind)
	}
	if env.Role != "user" {
		t.Errorf("Role: got %q, want user", env.Role)
	}
	if env.Cwd != "/work" {
		t.Errorf("Cwd: got %q", env.Cwd)
	}
	if env.ContentText != "investigate the slow query" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestAssembleGemini_AfterModelIsAssistantMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain string response": `{
			"session_id":"g-am-1",
			"hook_event_name":"AfterModel",
			"response":"the capital of japan is tokyo"
		}`,
		"wrapped {response:{text:...}}": `{
			"session_id":"g-am-2",
			"hook_event_name":"AfterModel",
			"response":{"text":"the capital of japan is tokyo"}
		}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, err := AssembleGemini([]byte(raw), time.Now().UTC())
			if err != nil {
				t.Fatalf("AssembleGemini: %v", err)
			}
			if env.Kind != "assistant_message" {
				t.Errorf("Kind: got %q, want assistant_message", env.Kind)
			}
			if env.Role != "assistant" {
				t.Errorf("Role: got %q, want assistant", env.Role)
			}
			if !strings.Contains(env.ContentText, "tokyo") {
				t.Errorf("ContentText: got %q, want substring tokyo", env.ContentText)
			}
		})
	}
}

func TestAssembleGemini_AfterToolWithoutErrorIsToolUse(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"g-tu",
		"hook_event_name":"AfterTool",
		"cwd":"/work",
		"tool_name":"run_shell_command",
		"tool_input":{"command":"ls -la"},
		"tool_response":{"llmContent":"...","returnDisplay":"..."}
	}`)
	env, err := AssembleGemini(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("AssembleGemini: %v", err)
	}
	if env.Kind != "tool_use" {
		t.Errorf("Kind: got %q, want tool_use", env.Kind)
	}
	if env.Tool == nil || env.Tool.Name != "run_shell_command" {
		t.Errorf("Tool: got %+v", env.Tool)
	}
	if !strings.Contains(env.ContentText, "ls -la") {
		t.Errorf("ContentText: got %q (want it to render the command)", env.ContentText)
	}
}

func TestAssembleGemini_AfterToolWithErrorMapsToToolFailure(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"error string": `{
			"session_id":"g-tf-1",
			"hook_event_name":"AfterTool",
			"tool_name":"run_shell_command",
			"tool_input":{"command":"false"},
			"tool_response":{"error":"exit code 1"}
		}`,
		"error object": `{
			"session_id":"g-tf-2",
			"hook_event_name":"AfterTool",
			"tool_name":"run_shell_command",
			"tool_input":{"command":"false"},
			"tool_response":{"error":{"message":"boom","type":"runtime"}}
		}`,
		"error true bool": `{
			"session_id":"g-tf-3",
			"hook_event_name":"AfterTool",
			"tool_name":"run_shell_command",
			"tool_input":{"command":"false"},
			"tool_response":{"error":true}
		}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, err := AssembleGemini([]byte(raw), time.Now().UTC())
			if err != nil {
				t.Fatalf("AssembleGemini: %v", err)
			}
			if env.Kind != "tool_failure" {
				t.Errorf("Kind: got %q, want tool_failure", env.Kind)
			}
			if env.Role != "tool" {
				t.Errorf("Role: got %q, want tool", env.Role)
			}
		})
	}
}

func TestAssembleGemini_AfterToolEmptyErrorStaysToolUse(t *testing.T) {
	t.Parallel()
	// An empty / null error must NOT trigger tool_failure — false
	// positives there inflate the staleness detector.
	cases := map[string]string{
		"error empty string": `{
			"session_id":"g-tu-e1",
			"hook_event_name":"AfterTool",
			"tool_name":"X",
			"tool_response":{"error":""}
		}`,
		"error null": `{
			"session_id":"g-tu-e2",
			"hook_event_name":"AfterTool",
			"tool_name":"X",
			"tool_response":{"error":null}
		}`,
		"error false bool": `{
			"session_id":"g-tu-e3",
			"hook_event_name":"AfterTool",
			"tool_name":"X",
			"tool_response":{"error":false}
		}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, err := AssembleGemini([]byte(raw), time.Now().UTC())
			if err != nil {
				t.Fatalf("AssembleGemini: %v", err)
			}
			if env.Kind != "tool_use" {
				t.Errorf("Kind: got %q, want tool_use (empty error must not trigger failure)", env.Kind)
			}
		})
	}
}

func TestAssembleGemini_SessionStartAndEnd(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"session_start": "SessionStart",
		"session_end":   "SessionEnd",
	}
	for wantKind, evt := range cases {
		t.Run(evt, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"session_id":"s1","hook_event_name":"` + evt + `"}`)
			env, err := AssembleGemini(raw, time.Now().UTC())
			if err != nil {
				t.Fatalf("AssembleGemini: %v", err)
			}
			if env.Kind != wantKind {
				t.Errorf("Kind: got %q, want %q", env.Kind, wantKind)
			}
		})
	}
}

func TestAssembleGemini_MissingSessionIDIsError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"BeforeAgent"}`)
	if _, err := AssembleGemini(raw, time.Now()); err == nil {
		t.Fatal("expected error for missing session_id")
	}
}

func TestAssembleGemini_UnknownEventMapsToUnknown(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"x","hook_event_name":"NotARealEvent"}`)
	env, err := AssembleGemini(raw, time.Now())
	if err != nil {
		t.Fatalf("AssembleGemini: %v", err)
	}
	if env.Kind != "unknown" {
		t.Errorf("Kind: got %q, want unknown", env.Kind)
	}
}
