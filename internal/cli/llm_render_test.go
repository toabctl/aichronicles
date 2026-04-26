package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/pkg/llm"
)

// TestParseToolResult_RejectsMultipleToolUses pins the exact-1
// contract: forced tool use means one call, period. Silently
// using ToolUses[0] would discard the rest and the user couldn't
// tell the model went off-rails.
func TestParseToolResult_RejectsMultipleToolUses(t *testing.T) {
	t.Parallel()
	resp := &llm.Response{
		ToolUses: []llm.ToolUse{
			{Name: "record_summary", Input: json.RawMessage(`{}`)},
			{Name: "record_summary", Input: json.RawMessage(`{}`)},
		},
	}
	var got struct{}
	err := parseToolResult(resp, "record_summary", &got)
	if err == nil {
		t.Fatal("expected error for >1 tool uses")
	}
	if !strings.Contains(err.Error(), "2 tool uses") {
		t.Errorf("error should report the count, got: %v", err)
	}
	if !strings.Contains(err.Error(), "record_summary") {
		t.Errorf("error should name the forced tool, got: %v", err)
	}
}

func TestParseToolResult_AcceptsExactlyOne(t *testing.T) {
	t.Parallel()
	resp := &llm.Response{
		ToolUses: []llm.ToolUse{
			{Name: "record_summary", Input: json.RawMessage(`{"topic":"x"}`)},
		},
	}
	var got struct {
		Topic string `json:"topic"`
	}
	if err := parseToolResult(resp, "record_summary", &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Topic != "x" {
		t.Errorf("decoded topic: got %q", got.Topic)
	}
}

func TestParseToolResult_ToolNameMismatchIsError(t *testing.T) {
	t.Parallel()
	resp := &llm.Response{
		ToolUses: []llm.ToolUse{
			{Name: "wrong_tool", Input: json.RawMessage(`{}`)},
		},
	}
	var got struct{}
	err := parseToolResult(resp, "record_summary", &got)
	if err == nil {
		t.Fatal("expected error for wrong tool name")
	}
	if !strings.Contains(err.Error(), "wrong_tool") || !strings.Contains(err.Error(), "record_summary") {
		t.Errorf("error should name both tools, got: %v", err)
	}
}
