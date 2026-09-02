package codex

import (
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
)

// TestTranslatedPayloadIsFullyScrubbable is the security-relevant
// property for this source. Codex puts tool output somewhere the
// other two agents do not: `tool_response` is a bare top-level
// STRING (Claude and Gemini both nest it in an object), and
// `tool_input.command` is the raw shell line. A secret echoed by a
// command therefore sits at a shape the payload walker has to
// handle as a plain string leaf, not a map.
//
// Translation itself is deliberately unredacted — the daemon
// scrubs server-side — so this drives the real redactor over a
// real translated envelope and asserts nothing survives.
func TestTranslatedPayloadIsFullyScrubbable(t *testing.T) {
	t.Parallel()
	const secret = "AKIAIOSFODNN7EXAMPLE"
	raw := `{"session_id":"s","cwd":"/w","hook_event_name":"PostToolUse",
		"tool_name":"Bash",
		"tool_input":{"command":"aws configure set aws_access_key_id ` + secret + `"},
		"tool_response":"AWS_ACCESS_KEY_ID=` + secret + `\n",
		"tool_use_id":"exec-1"}`

	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate([]byte(raw))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// Precondition: the translator must NOT have scrubbed anything
	// itself, or this test would pass for the wrong reason.
	if env.Redaction != nil {
		t.Fatalf("translator redacted; it must stay pure: %+v", env.Redaction)
	}

	events.ApplyRedaction(&env, redact.Default())

	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("redaction not marked applied: %+v", env.Redaction)
	}

	// tool_response — the bare-string leaf unique to Codex.
	resp, ok := env.Payload["tool_response"].(string)
	if !ok {
		t.Fatalf("tool_response is not a string after redaction: %T", env.Payload["tool_response"])
	}
	if strings.Contains(resp, secret) {
		t.Errorf("secret survived in tool_response: %q", resp)
	}
	if !strings.Contains(resp, "<redacted:aws_access_key>") {
		t.Errorf("expected redaction marker in tool_response: %q", resp)
	}

	// tool_input.command — nested one level, same as the others.
	input, _ := env.Payload["tool_input"].(map[string]any)
	cmd, _ := input["command"].(string)
	if strings.Contains(cmd, secret) {
		t.Errorf("secret survived in tool_input.command: %q", cmd)
	}

	// content_text is derived from tool_input at translate time, so
	// it carries its own copy of the command and needs its own scrub.
	if strings.Contains(env.ContentText, secret) {
		t.Errorf("secret survived in content_text: %q", env.ContentText)
	}
}

// TestPromptAndAssistantTextAreScrubbable covers the other two
// places Codex puts free text: `prompt` on UserPromptSubmit and
// `last_assistant_message` on Stop.
func TestPromptAndAssistantTextAreScrubbable(t *testing.T) {
	t.Parallel()
	const secret = "AKIAIOSFODNN7EXAMPLE"
	cases := []struct {
		name  string
		raw   string
		field string
	}{
		{
			name:  "UserPromptSubmit prompt",
			raw:   `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"my key is ` + secret + `"}`,
			field: "prompt",
		},
		{
			name:  "Stop last_assistant_message",
			raw:   `{"session_id":"s","hook_event_name":"Stop","last_assistant_message":"the key is ` + secret + `"}`,
			field: "last_assistant_message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &HookTranslator{Now: testNow}
			env, err := tr.Translate([]byte(tc.raw))
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			events.ApplyRedaction(&env, redact.Default())

			got, _ := env.Payload[tc.field].(string)
			if strings.Contains(got, secret) {
				t.Errorf("secret survived in payload.%s: %q", tc.field, got)
			}
			if strings.Contains(env.ContentText, secret) {
				t.Errorf("secret survived in content_text: %q", env.ContentText)
			}
		})
	}
}
