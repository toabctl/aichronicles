package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/llm"
)

// TestProviderLabel pins the strings the cli surfaces in command
// headers ("provider=anthropic", "provider=openai") so a future
// rename in internal/llm doesn't silently change what users see.
func TestProviderLabel(t *testing.T) {
	t.Parallel()
	cases := map[llm.Provider]string{
		"":                    "anthropic",
		llm.ProviderAnthropic: "anthropic",
		llm.ProviderOpenAI:    "openai",
		"weird-future":        "weird-future",
	}
	for in, want := range cases {
		got := providerLabel(llm.Config{Provider: in})
		if got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveModelLabel covers the model-name resolver used by
// command headers. flagModel wins when non-empty; otherwise the
// per-provider default constant is surfaced so the user sees the
// real model id (e.g. "claude-opus-4-7") rather than a generic
// "(provider default)" placeholder.
func TestResolveModelLabel(t *testing.T) {
	t.Parallel()
	type tc struct {
		cfg       llm.Config
		flagModel string
		want      string
	}
	cases := []tc{
		// flag wins regardless of provider.
		{llm.Config{Provider: llm.ProviderAnthropic}, "claude-opus-4-7", "claude-opus-4-7"},
		{llm.Config{Provider: llm.ProviderOpenAI}, "gpt-4.1", "gpt-4.1"},
		// Empty flag → provider default.
		{llm.Config{Provider: llm.ProviderAnthropic}, "", llm.DefaultAnthropicModel},
		{llm.Config{Provider: ""}, "", llm.DefaultAnthropicModel}, // empty == anthropic
		{llm.Config{Provider: llm.ProviderOpenAI}, "", llm.DefaultOpenAIModel},
	}
	for i, c := range cases {
		got := resolveModelLabel(c.cfg, c.flagModel)
		if got != c.want {
			t.Errorf("case %d (provider=%q flag=%q): got %q, want %q",
				i, c.cfg.Provider, c.flagModel, got, c.want)
		}
	}
}

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

// TestParseToolResult_RejectsMaxTokensTruncation pins the
// truncation guard: a reply that stopped at the token cap carries
// an unreliable tool input — even one that decodes into a clean
// zero-valued struct (the failure mode that cached an empty
// reflect_weekly digest). We must reject it rather than let the
// caller persist a hollow result under a cache key.
func TestParseToolResult_RejectsMaxTokensTruncation(t *testing.T) {
	t.Parallel()
	resp := &llm.Response{
		StopReason: llm.StopMaxTokens,
		// A well-formed-but-empty tool input that WOULD decode fine —
		// the guard must fire on stop reason, before trusting bytes.
		ToolUses: []llm.ToolUse{
			{Name: "record_reflection", Input: json.RawMessage(`{}`)},
		},
		Usage: llm.Usage{OutputTokens: 2048},
	}
	var got struct {
		Topic string `json:"topic"`
	}
	err := parseToolResult(resp, "record_reflection", &got)
	if err == nil {
		t.Fatal("expected error for max_tokens-truncated response")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error should name the truncation cause, got: %v", err)
	}
	if !strings.Contains(err.Error(), "2048") {
		t.Errorf("error should surface output_tokens for diagnosis, got: %v", err)
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
