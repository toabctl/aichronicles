package events

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/redact"
)

// freshEnv returns a minimal valid Envelope for tests to mutate.
func freshEnv() *Envelope {
	return &Envelope{
		V:               CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-1",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{},
	}
}

func TestApplyRedaction_ScrubsContentText(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.ContentText = "oops my key is sk-ant-" + strings.Repeat("a", 40)

	ApplyRedaction(env, redact.Default())

	if strings.Contains(env.ContentText, "sk-ant-") {
		t.Errorf("content_text still contains secret: %q", env.ContentText)
	}
	if !strings.Contains(env.ContentText, "<redacted:anthropic_api_key>") {
		t.Errorf("expected marker in content_text: %q", env.ContentText)
	}
	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("Redaction.Applied must be true")
	}
	if len(env.Redaction.Patterns) != 1 || env.Redaction.Patterns[0] != "anthropic_api_key" {
		t.Errorf("patterns: got %v", env.Redaction.Patterns)
	}
}

func TestApplyRedaction_ScrubsPayloadLeaves(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = map[string]any{
		"tool_input": map[string]any{
			"command":   "curl -H 'Authorization: Bearer " + strings.Repeat("z", 30) + "' https://api",
			"env":       []any{"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"},
			"unrelated": 42,
			"nil":       nil,
			"boolean":   true,
		},
	}

	ApplyRedaction(env, redact.Default())

	top := env.Payload["tool_input"].(map[string]any)
	cmd := top["command"].(string)
	if strings.Contains(cmd, "Bearer zzzz") {
		t.Errorf("bearer not scrubbed: %q", cmd)
	}
	if !strings.Contains(cmd, "<redacted:bearer_token>") {
		t.Errorf("expected bearer marker: %q", cmd)
	}
	envList := top["env"].([]any)
	if !strings.Contains(envList[0].(string), "<redacted:aws_access_key>") {
		t.Errorf("expected aws marker in array element: %v", envList[0])
	}
	if top["unrelated"] != 42 || top["boolean"] != true || top["nil"] != nil {
		t.Errorf("non-string leaves mutated: %+v", top)
	}

	pats := env.Redaction.Patterns
	want := []string{"aws_access_key", "bearer_token"}
	if !reflect.DeepEqual(pats, want) {
		t.Errorf("patterns: got %v want %v", pats, want)
	}
}

func TestApplyRedaction_NoFindings_StillMarksApplied(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.ContentText = "just a normal prompt with no secrets"
	env.Payload = map[string]any{"foo": "bar", "n": 1}

	ApplyRedaction(env, redact.Default())

	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("Applied must be true even without findings")
	}
	if len(env.Redaction.Patterns) != 0 {
		t.Errorf("patterns should be empty: got %v", env.Redaction.Patterns)
	}
	if env.ContentText != "just a normal prompt with no secrets" {
		t.Errorf("content_text mutated when no secrets: %q", env.ContentText)
	}
}

func TestApplyRedaction_NilPayload_Safe(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = nil
	env.ContentText = ""

	ApplyRedaction(env, redact.Default())

	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("Applied must be true")
	}
	if env.Payload != nil {
		t.Errorf("nil payload should remain nil, got %v", env.Payload)
	}
}

func TestApplyRedaction_UnionsPatternsAcrossFields(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.ContentText = "sk-ant-" + strings.Repeat("a", 40)
	env.Payload = map[string]any{
		"leaks": []any{
			"AIzaSyA-abcdefghijklmnopqrstuvwxyz12345",
			"AKIAIOSFODNN7EXAMPLE",
		},
	}

	ApplyRedaction(env, redact.Default())

	got := env.Redaction.Patterns
	want := []string{"anthropic_api_key", "aws_access_key", "google_api_key"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("union patterns: got %v want %v", got, want)
	}
}

func TestApplyRedaction_DeeplyNestedPayload(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = map[string]any{
		"a": map[string]any{
			"b": []any{
				map[string]any{
					"c": "token AKIAIOSFODNN7EXAMPLE buried deep",
				},
			},
		},
	}

	ApplyRedaction(env, redact.Default())

	a := env.Payload["a"].(map[string]any)
	b := a["b"].([]any)
	c := b[0].(map[string]any)
	leaf := c["c"].(string)
	if !strings.Contains(leaf, "<redacted:aws_access_key>") {
		t.Errorf("deep leaf not scrubbed: %q", leaf)
	}
}
