package gemini

import (
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
)

var testNow = func() time.Time { return time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC) }

func TestHookTranslator_BeforeAgentMapsToUserPrompt(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"s","hook_event_name":"BeforeAgent","prompt":"hi"}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.SourceAgent != "gemini-cli" {
		t.Errorf("SourceAgent: got %q, want gemini-cli", env.SourceAgent)
	}
	if env.Kind != events.KindUserPrompt {
		t.Errorf("Kind: got %q", env.Kind)
	}
	if env.ContentText != "hi" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestHookTranslator_AfterModelExtractsResponseString(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"s","hook_event_name":"AfterModel","response":"plain text"}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Kind != events.KindAssistantMessage {
		t.Errorf("Kind: got %q", env.Kind)
	}
	if env.ContentText != "plain text" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestHookTranslator_AfterModelExtractsResponseWrapped(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"s","hook_event_name":"AfterModel","response":{"text":"wrapped"}}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.ContentText != "wrapped" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestHookTranslator_AfterToolWithErrorBecomesToolFailure(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"s",
		"hook_event_name":"AfterTool",
		"tool_name":"run_shell_command",
		"tool_input":{"command":"false"},
		"tool_response":{"error":"exit 1"}
	}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Kind != events.KindToolFailure {
		t.Errorf("Kind: got %q, want %q", env.Kind, events.KindToolFailure)
	}
}

func TestHookTranslator_AfterToolNoErrorStaysToolUse(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"session_id":"s",
		"hook_event_name":"AfterTool",
		"tool_name":"run_shell_command",
		"tool_input":{"command":"echo ok"},
		"tool_response":{}
	}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Kind != events.KindToolUse {
		t.Errorf("Kind: got %q, want %q", env.Kind, events.KindToolUse)
	}
}

func TestHookTranslator_MissingSessionIDErrors(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"BeforeAgent"}`)
	tr := &HookTranslator{Now: testNow}
	if _, err := tr.Translate(raw); err == nil {
		t.Errorf("expected error for missing session_id")
	}
}

// TestHookTranslator_EmitsUnredactedEnvelope guards against a
// regression where the gemini translator re-acquires its own
// Redactor. Translation is pure; redaction is the consuming
// Pipeline's job. Mirror of the same property on the claude side.
func TestHookTranslator_EmitsUnredactedEnvelope(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"s","hook_event_name":"BeforeAgent","prompt":"hi"}`)
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate(raw)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if env.Redaction != nil {
		t.Errorf("Translator must emit unredacted envelopes; got Redaction=%+v", env.Redaction)
	}
}
