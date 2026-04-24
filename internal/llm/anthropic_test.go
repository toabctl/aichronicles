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
// the response the SUT parsed.
func fakeAnthropic(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Anthropic{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
}

func TestAnthropic_Complete_HappyPath(t *testing.T) {
	t.Parallel()
	var gotBody anthropicBody
	var gotHeaders http.Header

	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
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
	if gotBody.Model != "claude-sonnet-4-6" {
		t.Errorf("request model: got %q", gotBody.Model)
	}
	if gotBody.MaxTokens != 64 {
		t.Errorf("max_tokens: got %d", gotBody.MaxTokens)
	}
	if len(gotBody.System) != 1 || gotBody.System[0].Text != "be concise" {
		t.Errorf("system: got %+v", gotBody.System)
	}
	if gotBody.System[0].CacheControl == nil || gotBody.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("system cache_control: got %+v", gotBody.System[0].CacheControl)
	}
	if gotBody.System[0].Type != "text" {
		t.Errorf("system type: got %q, want \"text\"", gotBody.System[0].Type)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Role != "user" {
		t.Errorf("messages: got %+v", gotBody.Messages)
	}
	if gotHeaders.Get("x-api-key") != "test-key" {
		t.Errorf("x-api-key header missing: %v", gotHeaders)
	}
	if gotHeaders.Get("anthropic-version") != AnthropicAPIVersion {
		t.Errorf("anthropic-version header: got %q", gotHeaders.Get("anthropic-version"))
	}
}

func TestAnthropic_Complete_OmitsSystemBlockWhenEmpty(t *testing.T) {
	t.Parallel()
	// Round-trip the wire body through the raw map so we catch the
	// case where json.Marshal emits `"system": []` or `"system": ""`
	// instead of omitting the field entirely. Anthropic accepts either,
	// but the omitempty contract matters for cache-key stability.
	var raw map[string]json.RawMessage
	c := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		_, _ = io.WriteString(w,
			`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	if _, err := c.Complete(context.Background(), Request{
		// System omitted
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := raw["system"]; present {
		t.Errorf("system field should be omitted when empty, got %s", raw["system"])
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
		var body anthropicBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
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

func TestAnthropic_Complete_Non2xxErrorIncludesStatusAndBody(t *testing.T) {
	t.Parallel()
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	})
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
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("expected upstream body in error: %v", err)
	}
}

func TestAnthropic_Complete_ScrubsAPIKeyFromErrorBody(t *testing.T) {
	t.Parallel()
	// Simulate an upstream error response that echoes the key.
	leaked := "sk-ant-" + strings.Repeat("x", 40)
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid key: `+leaked+`"}}`)
	})
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
	c.RetryBaseDelay = time.Millisecond // keep the test fast

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
	c.RetryBaseDelay = time.Millisecond

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
	// 1 initial + 2 retries = 3 attempts.
	if got := calls.Load(); got != 3 {
		t.Errorf("calls: got %d, want 3", got)
	}
}

func TestAnthropic_Complete_DoesNotRetry4xxOtherThan429(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"malformed"}`)
	})
	c.RetryBaseDelay = time.Millisecond

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
	// Base delay tiny — if Retry-After is ignored, gap will be ~1ms,
	// not ~1s, and the assertion below catches it.
	c.RetryBaseDelay = time.Millisecond

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
	// And not >2s either — that'd mean we waited way too long
	// (defaultRetryMaxDelay is 10s so this is a sanity bound).
	if gap > 2*time.Second {
		t.Errorf("Retry-After=1 should not delay >2s, got %v", gap)
	}
}

func TestAnthropic_Complete_RetryAfterCapsAtMaxDelay(t *testing.T) {
	t.Parallel()
	// A server claiming "come back in an hour" must not hang us for
	// an hour — we cap at defaultRetryMaxDelay (10s) and still retry.
	if got, ok := parseRetryAfter("3600", time.Unix(0, 0)); !ok || got != time.Hour {
		t.Fatalf("parseRetryAfter integer path: got %v,%v", got, ok)
	}
	got := retryDelay("3600", 0, time.Millisecond)
	if got > defaultRetryMaxDelay {
		t.Errorf("retryDelay should cap at %v, got %v", defaultRetryMaxDelay, got)
	}
}

func TestAnthropic_Complete_RetryAfterHTTPDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	target := now.Add(3 * time.Second).Format(http.TimeFormat)
	got, ok := parseRetryAfter(target, now)
	if !ok {
		t.Fatalf("parseRetryAfter date path failed")
	}
	// Exact 3s, give or take a second from header-date truncation.
	if got < 2*time.Second || got > 4*time.Second {
		t.Errorf("HTTP-date delta: got %v, want ~3s", got)
	}
}

func TestAnthropic_Complete_ContextCancelledDuringBackoff(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "10") // long server-requested wait
		w.WriteHeader(http.StatusTooManyRequests)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel soon after the first attempt has surely landed.
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
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context cancellation, got %v", err)
	}
	// At most 1 network attempt should have happened before cancel.
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 attempt before cancel, got %d", got)
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
	c.RetryBaseDelay = time.Millisecond

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

func TestFromEnvOrCommand_EnvWins(t *testing.T) {
	// Not parallel: mutates process env.
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
	// A command that writes noise to stderr but a valid key to stdout
	// must still succeed — stderr is explicitly ignored so a chatty
	// keyring tool doesn't break the resolve.
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
