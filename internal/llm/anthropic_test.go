package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if gotBody.System != "be concise" {
		t.Errorf("system: got %q", gotBody.System)
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
