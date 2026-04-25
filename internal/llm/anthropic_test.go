package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAnthropic runs the handler as an HTTP server and returns a
// Client pointed at it. Tests assert on the request the SUT sent AND
// the response the SUT parsed. The SDK is configured against the
// fake's BaseURL/HTTP client, so the entire stack — serialization,
// retries, body parsing — exercises real SDK code.
//
// Sets Content-Type: application/json by default before invoking the
// handler so test bodies don't have to repeat the header — the SDK
// is strict about content-type and will refuse a JSON body served
// as text/plain.
func fakeAnthropic(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}
	srv := httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(srv.Close)
	return &Anthropic{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
}

// decodeRequestBody reads the raw POST body into a generic map so
// tests can inspect arbitrary JSON shape without coupling to wire
// types we no longer own (the SDK serializes them).
func decodeRequestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	return m
}

func TestAnthropic_Complete_HappyPath(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	var gotHeaders http.Header

	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody = decodeRequestBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_1","model":"claude-sonnet-4-6",
			"content":[{"type":"text","text":"hello back"}],
			"usage":{"input_tokens":12,"output_tokens":3}
		}`)
	})

	resp, err := c.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-6",
		System:    "be concise",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello back" {
		t.Errorf("Text: got %q", resp.Text)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage: got %+v", resp.Usage)
	}
	if gotBody["model"] != "claude-sonnet-4-6" {
		t.Errorf("request model: got %v", gotBody["model"])
	}
	// max_tokens: JSON unmarshals integers as float64; SDK sends 64.
	if mt, _ := gotBody["max_tokens"].(float64); mt != 64 {
		t.Errorf("max_tokens: got %v", gotBody["max_tokens"])
	}
	// system: SDK serializes as an array of text blocks with cache_control.
	sysArr, ok := gotBody["system"].([]any)
	if !ok || len(sysArr) != 1 {
		t.Fatalf("system: expected single-element array, got %v", gotBody["system"])
	}
	sysBlock := sysArr[0].(map[string]any)
	if sysBlock["text"] != "be concise" {
		t.Errorf("system text: got %v", sysBlock["text"])
	}
	cc, ok := sysBlock["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("system cache_control should be ephemeral, got %v", sysBlock["cache_control"])
	}
	if gotHeaders.Get("x-api-key") != "test-key" {
		t.Errorf("x-api-key header missing: %v", gotHeaders)
	}
}

func TestAnthropic_Complete_ConcatenatesTextBlocks(t *testing.T) {
	t.Parallel()
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"content":[
				{"type":"text","text":"part one "},
				{"type":"text","text":"part two"}
			],
			"usage":{"input_tokens":1,"output_tokens":2}
		}`)
	})
	resp, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "part one part two" {
		t.Errorf("concatenation: got %q", resp.Text)
	}
}

func TestAnthropic_Complete_DefaultsModelWhenEmpty(t *testing.T) {
	t.Parallel()
	var gotModel string
	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r)
		gotModel, _ = body["model"].(string)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	if _, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotModel != DefaultAnthropicModel {
		t.Errorf("default model: got %q, want %q", gotModel, DefaultAnthropicModel)
	}
}

func TestAnthropic_Complete_Non2xxErrorIncludesStatus(t *testing.T) {
	t.Parallel()
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	})
	c.MaxRetries = -1 // don't waste test time on retries we don't care about
	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error: %v", err)
	}
}

func TestAnthropic_Complete_ScrubsAPIKeyFromErrorBody(t *testing.T) {
	t.Parallel()
	leaked := "sk-ant-" + strings.Repeat("x", 40)
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid key: `+leaked+`"}}`)
	})
	c.MaxRetries = -1
	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), leaked) {
		t.Fatalf("API key leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted:anthropic_api_key>") {
		t.Errorf("expected redaction marker in error: %v", err)
	}
}

func TestAnthropic_Complete_RefusesEmptyAPIKey(t *testing.T) {
	t.Parallel()
	a := &Anthropic{APIKey: ""}
	_, err := a.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAnthropic_Complete_RetriesOn429ThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w,
			`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	resp, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text: %q", resp.Text)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls: got %d, want 3 (two 429s then success)", got)
	}
}

func TestAnthropic_Complete_RetriesOn5xxThenFails(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	})
	c.MaxRetries = 2

	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention 503: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls: got %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestAnthropic_Complete_DoesNotRetry400(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"malformed"}`)
	})

	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected 400 error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("400 must not retry: got %d attempts, want 1", got)
	}
}

func TestAnthropic_Complete_HonorsRetryAfterSeconds(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var firstAt, secondAt time.Time
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			firstAt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		secondAt = time.Now()
		_, _ = io.WriteString(w,
			`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	if _, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	gap := secondAt.Sub(firstAt)
	if gap < 750*time.Millisecond {
		t.Errorf("Retry-After=1 should delay ≥~1s, got %v", gap)
	}
	if gap > 2*time.Second {
		t.Errorf("Retry-After=1 should not delay >2s, got %v", gap)
	}
}

func TestAnthropic_Complete_ContextCancelledAborts(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "10") // long server-requested wait
		w.WriteHeader(http.StatusTooManyRequests)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := c.Complete(ctx, Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected context error")
	}
	// SDK wraps context cancellation; error message contains "context".
	if !strings.Contains(strings.ToLower(err.Error()), "context") {
		t.Errorf("expected context cancellation diagnostic, got %v", err)
	}
}

func TestAnthropic_Complete_MaxRetriesNegativeDisables(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.MaxRetries = -1

	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected 429 error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("MaxRetries=-1 should make exactly 1 attempt, got %d", got)
	}
}

func TestAnthropic_Complete_SendsToolsAndToolChoice(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequestBody(t, r)
		_, _ = io.WriteString(w, `{
			"content":[{"type":"tool_use","id":"toolu_1","name":"record_x","input":{"k":"v"}}],
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	})

	schema := json.RawMessage(`{"type":"object","properties":{"k":{"type":"string"}},"required":["k"]}`)
	resp, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
		Tools: []Tool{
			{Name: "record_x", Description: "does x", InputSchema: schema},
		},
		ForceTool: "record_x",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: expected 1, got %v", gotBody["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "record_x" {
		t.Errorf("tool name: got %v", tool["name"])
	}
	is, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema not present: %v", tool)
	}
	if is["type"] != "object" {
		t.Errorf("schema type: got %v", is["type"])
	}
	if reqArr, _ := is["required"].([]any); len(reqArr) != 1 || reqArr[0] != "k" {
		t.Errorf("required: got %v", is["required"])
	}
	tc, ok := gotBody["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing: %v", gotBody)
	}
	if tc["type"] != "tool" || tc["name"] != "record_x" {
		t.Errorf("tool_choice: got %v", tc)
	}
	// Response side: tool_use surfaces.
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "record_x" {
		t.Errorf("tool_uses: got %+v", resp.ToolUses)
	}
	var decoded struct {
		K string `json:"k"`
	}
	if err := json.Unmarshal(resp.ToolUses[0].Input, &decoded); err != nil || decoded.K != "v" {
		t.Errorf("input round-trip: %+v err=%v", decoded, err)
	}
}

func TestAnthropic_Complete_OmitsToolFieldsWhenUnused(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequestBody(t, r)
		_, _ = io.WriteString(w,
			`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	if _, err := c.Complete(context.Background(), Request{
		System:    "be terse",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := gotBody["tools"]; present {
		t.Errorf("tools must be omitted when unused, got %v", gotBody["tools"])
	}
	if _, present := gotBody["tool_choice"]; present {
		t.Errorf("tool_choice must be omitted when unused, got %v", gotBody["tool_choice"])
	}
}

func TestAnthropic_Complete_MixedTextAndToolUseBlocks(t *testing.T) {
	t.Parallel()
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"content":[
				{"type":"text","text":"I'll record that now."},
				{"type":"tool_use","id":"toolu_a","name":"record_x","input":{"a":1}}
			],
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	})
	resp, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "I'll record that now." {
		t.Errorf("Text: got %q", resp.Text)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "record_x" {
		t.Errorf("ToolUses: got %+v", resp.ToolUses)
	}
}

func TestAnthropic_Complete_OmitsSystemBlockWhenEmpty(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequestBody(t, r)
		_, _ = io.WriteString(w,
			`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	if _, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := gotBody["system"]; present {
		t.Errorf("system field should be omitted when empty, got %v", gotBody["system"])
	}
}

// --- API key resolution (provider-neutral but tested here because
// the constructors live in this file) ---

func TestFromEnvOrCommand_EnvWins(t *testing.T) {
	t.Setenv(APIKeyEnv, "env-key")
	c, err := FromEnvOrCommand(context.Background(), "echo command-should-not-run")
	if err != nil {
		t.Fatalf("FromEnvOrCommand: %v", err)
	}
	a, ok := c.(*Anthropic)
	if !ok {
		t.Fatalf("unexpected client type: %T", c)
	}
	if a.APIKey != "env-key" {
		t.Errorf("env should win: got %q", a.APIKey)
	}
}

func TestFromEnvOrCommand_FallsBackToCommand(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	c, err := FromEnvOrCommand(context.Background(), "printf 'file-key\n'")
	if err != nil {
		t.Fatalf("FromEnvOrCommand: %v", err)
	}
	a := c.(*Anthropic)
	if a.APIKey != "file-key" {
		t.Errorf("command output: got %q, want 'file-key' (trailing newline stripped)", a.APIKey)
	}
}

func TestFromEnvOrCommand_NeitherConfigured(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	_, err := FromEnvOrCommand(context.Background(), "")
	if err == nil {
		t.Fatal("expected error when both env and command are empty")
	}
	if !strings.Contains(err.Error(), APIKeyEnv) {
		t.Errorf("error should name the env var: %v", err)
	}
}

func TestFromEnvOrCommand_EmptyCommandOutputIsRejected(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	_, err := FromEnvOrCommand(context.Background(), "printf ''")
	if err == nil {
		t.Fatal("expected error for empty command output")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty output: %v", err)
	}
}

func TestFromEnvOrCommand_FailingCommandSurfaces(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	_, err := FromEnvOrCommand(context.Background(), "false")
	if err == nil {
		t.Fatal("expected error when command exits non-zero")
	}
	if !strings.Contains(err.Error(), "api_key_command") {
		t.Errorf("error should mention the source: %v", err)
	}
}

func TestFromEnvOrCommand_StderrIsDiscarded(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	c, err := FromEnvOrCommand(context.Background(),
		"printf 'unlocking keyring...\n' 1>&2; printf 'stdout-key'")
	if err != nil {
		t.Fatalf("FromEnvOrCommand: %v", err)
	}
	if c.(*Anthropic).APIKey != "stdout-key" {
		t.Errorf("got %q", c.(*Anthropic).APIKey)
	}
}

func TestFromEnvOrCommand_CancelledContextAborts(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FromEnvOrCommand(ctx, "sleep 30")
	if err == nil {
		t.Fatal("expected error from cancelled ctx")
	}
}

func TestValidateRequest_Rejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"no messages", Request{MaxTokens: 16}, "empty"},
		{"first not user", Request{
			Messages:  []Message{{Role: RoleAssistant, Content: "x"}},
			MaxTokens: 16,
		}, "user turn"},
		{"zero max tokens", Request{
			Messages: []Message{{Role: RoleUser, Content: "x"}},
		}, "MaxTokens"},
		{"bad role", Request{
			Messages: []Message{
				{Role: RoleUser, Content: "x"},
				{Role: "system", Content: "y"},
			},
			MaxTokens: 16,
		}, "not recognised"},
		{"empty content", Request{
			Messages:  []Message{{Role: RoleUser, Content: ""}},
			MaxTokens: 16,
		}, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRequest(tc.req)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
