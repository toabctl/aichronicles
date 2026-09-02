package codex

import (
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
)

var testNow = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }

func translate(t *testing.T, raw string) events.Envelope {
	t.Helper()
	tr := &HookTranslator{Now: testNow}
	env, err := tr.Translate([]byte(raw))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return env
}

// TestHookTranslator_LivePayloads drives the translator with hook
// payloads captured verbatim from codex-cli 0.149.1 (paths and ids
// preserved, only shortened). They are the contract: if Codex
// changes a field name, these break rather than the translator
// silently degrading to KindUnknown / empty content_text.
func TestHookTranslator_LivePayloads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		raw         string
		wantKind    string
		wantContent string
		wantTool    string
		wantCwd     string
	}{
		{
			name: "SessionStart",
			raw: `{"session_id":"01a060a9-2017-7ec1-9ffc-19951ea1cdac",
				"transcript_path":"/tmp/cx/sessions/2026/09/02/rollout-x.jsonl",
				"cwd":"/tmp/cx/work","hook_event_name":"SessionStart",
				"model":"gpt-5.6-sol","permission_mode":"bypassPermissions",
				"source":"startup"}`,
			wantKind: events.KindSessionStart,
			wantCwd:  "/tmp/cx/work",
		},
		{
			name: "UserPromptSubmit carries prompt",
			raw: `{"session_id":"s","turn_id":"t","cwd":"/tmp/cx/work",
				"hook_event_name":"UserPromptSubmit","model":"gpt-5.6-sol",
				"permission_mode":"bypassPermissions",
				"prompt":"Run the shell command 'cat note.txt'"}`,
			wantKind:    events.KindUserPrompt,
			wantContent: "Run the shell command 'cat note.txt'",
			wantCwd:     "/tmp/cx/work",
		},
		{
			// Codex reports a shell call in Claude Code's tool
			// vocabulary: tool_name "Bash", tool_input {"command":…}
			// — not its own internal "shell"/"exec" tool. That is
			// what makes events.RenderToolContent work unchanged.
			name: "PostToolUse shell uses Claude tool naming",
			raw: `{"session_id":"s","turn_id":"t","cwd":"/tmp/cx/work",
				"hook_event_name":"PostToolUse","model":"gpt-5.6-sol",
				"permission_mode":"bypassPermissions","tool_name":"Bash",
				"tool_input":{"command":"cat note.txt"},
				"tool_response":"hello world\n",
				"tool_use_id":"exec-51398ca3-cd31-40bd-9877-d1fb9fbd687f"}`,
			wantKind:    events.KindToolUse,
			wantContent: "Bash cat note.txt",
			wantTool:    "Bash",
			wantCwd:     "/tmp/cx/work",
		},
		{
			name: "PostToolUse apply_patch renders touched paths",
			raw: `{"session_id":"s","hook_event_name":"PostToolUse",
				"tool_name":"apply_patch",
				"tool_input":{"command":"*** Begin Patch\n*** Add File: /tmp/cx/work/foo.txt\n+bar\n*** End Patch"},
				"tool_response":"Exit code: 0\nWall time: 0 seconds\n"}`,
			wantKind:    events.KindToolUse,
			wantContent: "apply_patch /tmp/cx/work/foo.txt",
			wantTool:    "apply_patch",
		},
		{
			name: "Stop carries last_assistant_message",
			raw: `{"session_id":"s","turn_id":"t","cwd":"/tmp/cx/work",
				"hook_event_name":"Stop","model":"gpt-5.6-sol",
				"permission_mode":"bypassPermissions","stop_hook_active":false,
				"last_assistant_message":"hello world"}`,
			wantKind:    events.KindAssistantMessage,
			wantContent: "hello world",
			wantCwd:     "/tmp/cx/work",
		},
		{
			// Codex's SessionEnd input drops model and
			// permission_mode and pins reason to "other".
			name: "SessionEnd",
			raw: `{"session_id":"s","cwd":"/tmp/cx/work",
				"hook_event_name":"SessionEnd","reason":"other",
				"transcript_path":"/tmp/cx/sessions/2026/09/02/rollout-x.jsonl"}`,
			wantKind: events.KindSessionEnd,
			wantCwd:  "/tmp/cx/work",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := translate(t, tc.raw)
			if env.SourceAgent != "codex-cli" {
				t.Errorf("SourceAgent: got %q, want codex-cli", env.SourceAgent)
			}
			if env.Kind != tc.wantKind {
				t.Errorf("Kind: got %q, want %q", env.Kind, tc.wantKind)
			}
			if env.ContentText != tc.wantContent {
				t.Errorf("ContentText: got %q, want %q", env.ContentText, tc.wantContent)
			}
			if env.Cwd != tc.wantCwd {
				t.Errorf("Cwd: got %q, want %q", env.Cwd, tc.wantCwd)
			}
			switch {
			case tc.wantTool == "" && env.Tool != nil:
				t.Errorf("Tool: got %+v, want nil", env.Tool)
			case tc.wantTool != "" && env.Tool == nil:
				t.Errorf("Tool: got nil, want %q", tc.wantTool)
			case tc.wantTool != "" && env.Tool.Name != tc.wantTool:
				t.Errorf("Tool.Name: got %q, want %q", env.Tool.Name, tc.wantTool)
			}
			if env.Transport != "hook" {
				t.Errorf("Transport: got %q, want hook", env.Transport)
			}
		})
	}
}

// TestHookTranslator_ToolFailureIsNeverInferred pins the deliberate
// decision NOT to classify Codex tool failures. Codex's tool_response
// is the raw tool output with no exit code and no error field: a
// failing `cat` produces an ordinary-looking string. Guessing here
// would write confidently-wrong tool_failure rows, so a failed call
// is recorded as a plain tool_use with its output intact.
func TestHookTranslator_ToolFailureIsNeverInferred(t *testing.T) {
	t.Parallel()
	raw := `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash",
		"tool_input":{"command":"cat definitely-missing.txt"},
		"tool_response":"cat: definitely-missing.txt: No such file or directory\n"}`
	env := translate(t, raw)
	if env.Kind != events.KindToolUse {
		t.Errorf("Kind: got %q, want %q (failures must not be inferred)", env.Kind, events.KindToolUse)
	}
}

func TestHookTranslator_SubagentIdentity(t *testing.T) {
	t.Parallel()
	t.Run("agent_id present", func(t *testing.T) {
		t.Parallel()
		env := translate(t, `{"session_id":"s","hook_event_name":"SubagentStop",
			"agent_id":"sub-1","agent_type":"reviewer"}`)
		if env.Kind != events.KindSubagentStop {
			t.Errorf("Kind: got %q", env.Kind)
		}
		if env.Subagent == nil {
			t.Fatalf("Subagent: got nil, want {sub-1 reviewer}")
		}
		if env.Subagent.ID != "sub-1" || env.Subagent.Type != "reviewer" {
			t.Errorf("Subagent: got %+v", env.Subagent)
		}
	})
	t.Run("agent_type without agent_id is dropped", func(t *testing.T) {
		t.Parallel()
		env := translate(t, `{"session_id":"s","hook_event_name":"PostToolUse",
			"tool_name":"Bash","agent_type":"reviewer"}`)
		if env.Subagent != nil {
			t.Errorf("Subagent: got %+v, want nil", env.Subagent)
		}
	})
}

// TestHookTranslator_UnknownEventIsUnknownKind keeps a Codex event
// we have not modelled (PermissionRequest, say) observable instead
// of fatal.
func TestHookTranslator_UnknownEventIsUnknownKind(t *testing.T) {
	t.Parallel()
	env := translate(t, `{"session_id":"s","hook_event_name":"PermissionRequest"}`)
	if env.Kind != events.KindUnknown {
		t.Errorf("Kind: got %q, want %q", env.Kind, events.KindUnknown)
	}
}

func TestHookTranslator_MissingSessionIDErrors(t *testing.T) {
	t.Parallel()
	tr := &HookTranslator{Now: testNow}
	if _, err := tr.Translate([]byte(`{"hook_event_name":"UserPromptSubmit"}`)); err == nil {
		t.Errorf("expected error for missing session_id")
	}
}

func TestHookTranslator_MalformedJSONErrors(t *testing.T) {
	t.Parallel()
	tr := &HookTranslator{Now: testNow}
	if _, err := tr.Translate([]byte(`{not json`)); err == nil {
		t.Errorf("expected error for malformed payload")
	}
}

// TestHookTranslator_EmitsUnredactedEnvelope guards against a
// regression where the codex translator re-acquires its own
// Redactor. Translation is pure; redaction is the consuming
// Pipeline's job. Mirror of the same property on claude and gemini.
func TestHookTranslator_EmitsUnredactedEnvelope(t *testing.T) {
	t.Parallel()
	env := translate(t, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"hi"}`)
	if env.Redaction != nil {
		t.Errorf("Translator must emit unredacted envelopes; got Redaction=%+v", env.Redaction)
	}
}
