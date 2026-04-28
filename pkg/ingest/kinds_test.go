package ingest

import "testing"

func TestIsValidKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{KindUserPrompt, true},
		{KindAssistantMessage, true},
		{KindToolUse, true},
		{KindToolResult, true},
		{KindToolFailure, true},
		{KindSessionStart, true},
		{KindSessionEnd, true},
		{KindSubagentStart, true},
		{KindSubagentStop, true},
		{KindCompactStart, true},
		{KindCompactEnd, true},
		{KindCwdChanged, true},
		{KindInstructionsLoaded, true},
		{KindSystemMessage, true},
		{KindError, true},
		{KindUnknown, true},
		{"", false},
		{"tool_us", false}, // typo guard
		{"USER_PROMPT", false},
		{"summary", false}, // an llm_outputs kind, not an event kind
	}
	for _, c := range cases {
		if got := IsValidKind(c.in); got != c.want {
			t.Errorf("IsValidKind(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsValidRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{RoleUser, true},
		{RoleAssistant, true},
		{RoleTool, true},
		{RoleSystem, true},
		{"", false},
		{"USER", false},
		{"assistant ", false}, // whitespace not stripped
	}
	for _, c := range cases {
		if got := IsValidRole(c.in); got != c.want {
			t.Errorf("IsValidRole(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestKindConstantsMatchOpenAPI is a regression guard: the Kind*
// constants must stay in lockstep with api/openapi.yaml's enum.
// Failure here means someone added a kind without bumping both.
func TestKindConstantsMatchOpenAPI(t *testing.T) {
	t.Parallel()
	want := []string{
		"user_prompt",
		"assistant_message",
		"tool_use",
		"tool_result",
		"tool_failure",
		"session_start",
		"session_end",
		"subagent_start",
		"subagent_stop",
		"compact_start",
		"compact_end",
		"cwd_changed",
		"instructions_loaded",
		"system_message",
		"error",
		"unknown",
	}
	if len(validKinds) != len(want) {
		t.Fatalf("validKinds size = %d, want %d (sync with api/openapi.yaml)", len(validKinds), len(want))
	}
	for _, k := range want {
		if _, ok := validKinds[k]; !ok {
			t.Errorf("validKinds missing %q", k)
		}
	}
}
