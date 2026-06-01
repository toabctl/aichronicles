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
)

// fakeOpenAI mirrors fakeAnthropic: spin a httptest server, point
// the SDK at it via BaseURL/HTTPClient, and let the wrapper assert
// on whatever the SDK serialized.
//
// Sets Content-Type: application/json by default since the SDK
// rejects bodies served as text/plain.
func fakeOpenAI(t *testing.T, handler http.HandlerFunc) *OpenAI {
	t.Helper()
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}
	srv := httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(srv.Close)
	return &OpenAI{
		APIKey:   "test-openai-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
}

func decodeOpenAIBody(t *testing.T, r *http.Request) map[string]any {
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

func TestOpenAI_Complete_HappyPath(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	var gotHeaders http.Header
	c := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody = decodeOpenAIBody(t, r)
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl_1",
			"model":"gpt-4o-mini",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"hello back","refusal":""},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}
		}`)
	})

	resp, err := c.Complete(context.Background(), Request{
		Model:     "gpt-4o-mini",
		System:    "be concise",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello back" {
		t.Errorf("Text: %q", resp.Text)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("request model: %v", gotBody["model"])
	}
	if mt, _ := gotBody["max_completion_tokens"].(float64); mt != 64 {
		t.Errorf("max_completion_tokens: %v", gotBody["max_completion_tokens"])
	}
	// system prompt rides as the first message with role="system".
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages: expected 2 (system + user), got %v", gotBody["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be concise" {
		t.Errorf("system message: got %+v", first)
	}
	second := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "hi" {
		t.Errorf("user message: got %+v", second)
	}
	if gotHeaders.Get("Authorization") != "Bearer test-openai-key" {
		t.Errorf("Authorization header missing/wrong: %q", gotHeaders.Get("Authorization"))
	}
}

// TestOpenAI_Complete_MapsFinishReason mirrors the Anthropic
// stop-reason test: OpenAI's finish_reason "length" is the
// token-cap truncation we surface as StopMaxTokens so the shared
// truncation guard fires for either provider.
func TestOpenAI_Complete_MapsFinishReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"length", StopMaxTokens},
		{"tool_calls", StopToolUse},
		{"stop", StopOther},
		{"content_filter", StopOther},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			c := fakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{
					"choices":[{
						"index":0,
						"message":{"role":"assistant","content":"x","refusal":""},
						"finish_reason":"`+tc.raw+`"
					}],
					"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
				}`)
			})
			resp, err := c.Complete(context.Background(), Request{
				Messages:  []Message{{Role: RoleUser, Content: "x"}},
				MaxTokens: 16,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != tc.want {
				t.Errorf("finish_reason %q: got %q, want %q", tc.raw, resp.StopReason, tc.want)
			}
		})
	}
}

func TestOpenAI_Complete_DefaultsModelWhenEmpty(t *testing.T) {
	t.Parallel()
	var gotModel string
	c := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeOpenAIBody(t, r)
		gotModel, _ = body["model"].(string)
		_, _ = io.WriteString(w, `{
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok","refusal":""},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	})
	if _, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotModel != DefaultOpenAIModel {
		t.Errorf("default model: got %q, want %q", gotModel, DefaultOpenAIModel)
	}
}

func TestOpenAI_Complete_SendsToolsAndForcedToolChoice(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	c := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeOpenAIBody(t, r)
		_, _ = io.WriteString(w, `{
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant","content":"","refusal":"",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"record_x","arguments":"{\"k\":\"v\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	})

	schema := json.RawMessage(`{"type":"object","properties":{"k":{"type":"string"}},"required":["k"],"additionalProperties":false}`)
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
		t.Fatalf("tools: got %v", gotBody["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type: %v", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "record_x" {
		t.Errorf("function name: %v", fn["name"])
	}
	// Strict mode is intentionally NOT set: our tool schemas have
	// optional top-level fields (skill, workflow, …) that OpenAI's
	// strict mode would reject because they aren't in `required`.
	// json.Unmarshal into the typed result struct is the real
	// validation gate.
	if _, present := fn["strict"]; present {
		t.Errorf("strict should not appear in tool definition; got %v", fn["strict"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("parameters not parsed: %v", fn["parameters"])
	}
	tc := gotBody["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Errorf("tool_choice type: %v", tc["type"])
	}
	if tcFn := tc["function"].(map[string]any); tcFn["name"] != "record_x" {
		t.Errorf("tool_choice function name: %v", tcFn["name"])
	}

	// Response: arguments arrive as a JSON-encoded string; our wrapper
	// surfaces them as RawMessage and the caller Unmarshal's into
	// their typed result.
	if len(resp.ToolUses) != 1 {
		t.Fatalf("tool_uses: got %d", len(resp.ToolUses))
	}
	if resp.ToolUses[0].Name != "record_x" || resp.ToolUses[0].ID != "call_1" {
		t.Errorf("tool_use ident: %+v", resp.ToolUses[0])
	}
	var decoded struct {
		K string `json:"k"`
	}
	if err := json.Unmarshal(resp.ToolUses[0].Input, &decoded); err != nil {
		t.Fatalf("input unmarshal: %v", err)
	}
	if decoded.K != "v" {
		t.Errorf("input value: got %+v", decoded)
	}
}

func TestOpenAI_Complete_OmitsToolFieldsWhenUnused(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	c := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeOpenAIBody(t, r)
		_, _ = io.WriteString(w, `{
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok","refusal":""},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	})
	if _, err := c.Complete(context.Background(), Request{
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

func TestOpenAI_Complete_ScrubsAPIKeyFromErrorBody(t *testing.T) {
	t.Parallel()
	leaked := "sk-" + strings.Repeat("x", 50)
	c := fakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid key: `+leaked+`","code":"invalid_api_key"}}`)
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
	if !strings.Contains(err.Error(), "<redacted:openai_api_key>") {
		t.Errorf("expected redaction marker in error, got: %v", err)
	}
}

func TestOpenAI_Complete_RetriesOn5xxThenFails(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
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

// TestOpenAI_Complete_ForceToolDisablesRetries mirrors the
// Anthropic counterpart: a 429 on a tool-forced call must not
// retry, regardless of the client-wide MaxRetries setting. The
// CompletionUsage on the returned response reflects only the
// final attempt, so retried tool calls under-count input tokens
// and silently re-pay any cache prefix on every attempt.
func TestOpenAI_Complete_ForceToolDisablesRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow"}}`)
	})
	// Leave MaxRetries at the default; per-call override must win.

	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "x"}},
		MaxTokens: 16,
		Tools:     []Tool{{Name: "record_x", InputSchema: []byte(`{"type":"object"}`)}},
		ForceTool: "record_x",
	})
	if err == nil {
		t.Fatal("expected 429 error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("ForceTool should produce exactly 1 attempt regardless of MaxRetries, got %d", got)
	}
}

func TestOpenAI_Complete_RefusesEmptyAPIKey(t *testing.T) {
	t.Parallel()
	o := &OpenAI{APIKey: ""}
	_, err := o.Complete(context.Background(), Request{
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

// --- API key resolution ---

func TestFromEnvOrCommandOpenAI_EnvWins(t *testing.T) {
	t.Setenv(OpenAIAPIKeyEnv, "env-oai-key")
	c, err := FromEnvOrCommandOpenAI(context.Background(), "echo nope")
	if err != nil {
		t.Fatalf("FromEnvOrCommandOpenAI: %v", err)
	}
	o, ok := c.(*OpenAI)
	if !ok {
		t.Fatalf("type: %T", c)
	}
	if o.APIKey != "env-oai-key" {
		t.Errorf("env should win: %q", o.APIKey)
	}
}

func TestFromEnvOrCommandOpenAI_FallsBackToCommand(t *testing.T) {
	t.Setenv(OpenAIAPIKeyEnv, "")
	c, err := FromEnvOrCommandOpenAI(context.Background(), "printf 'cmd-oai-key'")
	if err != nil {
		t.Fatalf("FromEnvOrCommandOpenAI: %v", err)
	}
	if c.(*OpenAI).APIKey != "cmd-oai-key" {
		t.Errorf("got %q", c.(*OpenAI).APIKey)
	}
}

func TestFromEnvOrCommandOpenAI_NeitherConfigured(t *testing.T) {
	t.Setenv(OpenAIAPIKeyEnv, "")
	_, err := FromEnvOrCommandOpenAI(context.Background(), "")
	if err == nil {
		t.Fatal("expected error when both env and command are empty")
	}
	if !strings.Contains(err.Error(), OpenAIAPIKeyEnv) {
		t.Errorf("error should name the env var: %v", err)
	}
}
