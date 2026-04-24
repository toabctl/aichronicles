package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// AnthropicEndpoint is the Messages API URL. Exported so tests can
// override it to point at httptest.
const AnthropicEndpoint = "https://api.anthropic.com/v1/messages"

// AnthropicAPIVersion is pinned at the version we've tested against.
// Older versions dropped or renamed fields we depend on; newer versions
// should be adopted with an explicit bump rather than "whatever today's
// default is".
const AnthropicAPIVersion = "2023-06-01"

// DefaultAnthropicModel is the identifier Block B features fall back
// to when the user has not specified one. Kept conservative; callers
// can override per-request via Request.Model.
const DefaultAnthropicModel = "claude-sonnet-4-6"

// Anthropic is a Client that talks to the Anthropic Messages API.
// Safe for concurrent use: http.Client is goroutine-safe and this
// struct holds no other state.
type Anthropic struct {
	APIKey   string
	Endpoint string       // overridable for tests; empty means AnthropicEndpoint
	HTTP     *http.Client // optional; nil means a sensible default
}

// APIKeyEnv is the environment variable the CLI subcommands (and
// `FromEnv`) read when wiring up a production Anthropic client.
// Exported so user-facing docs can reference it without hard-coding
// the string.
const APIKeyEnv = "ANTHROPIC_API_KEY"

// FromEnv returns a production Anthropic client built from
// $ANTHROPIC_API_KEY. Returns a non-nil error when the env var is
// missing; callers in CLI-land surface that as a user-facing error.
// Tests wanting a different key path should use t.Setenv or construct
// NewAnthropic directly rather than calling through here.
func FromEnv() (Client, error) {
	key := os.Getenv(APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("llm: %s not set", APIKeyEnv)
	}
	return NewAnthropic(key), nil
}

// NewAnthropic returns a Client ready to hit production. The API key
// is required; an empty string is rejected at call time, not here,
// so tests that never actually issue a request can skip wiring one up.
func NewAnthropic(apiKey string) *Anthropic {
	return &Anthropic{
		APIKey: apiKey,
		HTTP: &http.Client{
			// Summaries + reflections can take tens of seconds at
			// the API end. 2 minutes is generous; the caller can set
			// a tighter context deadline if they want to cap faster.
			Timeout: 2 * time.Minute,
		},
	}
}

// Complete issues one Messages API call and returns the reply text
// plus token usage.
func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	if a.APIKey == "" {
		return nil, errors.New("anthropic: API key not set (expected in ANTHROPIC_API_KEY)")
	}
	if err := validateRequest(req); err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	model := req.Model
	if model == "" {
		model = DefaultAnthropicModel
	}

	body, err := buildAnthropicBody(req, model)
	if err != nil {
		return nil, err
	}

	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = AnthropicEndpoint
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", AnthropicAPIVersion)

	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("anthropic: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, truncate(respBody, 1024))
	}

	return parseAnthropicResponse(respBody, model)
}

// validateRequest enforces the small set of invariants we care about
// pre-flight. Provider-specific validation (e.g. role alternation)
// happens at encode time.
func validateRequest(req Request) error {
	if len(req.Messages) == 0 {
		return errors.New("Request.Messages is empty")
	}
	if req.Messages[0].Role != RoleUser {
		return errors.New("Request.Messages must start with a user turn")
	}
	if req.MaxTokens <= 0 {
		return errors.New("Request.MaxTokens must be positive")
	}
	for i, m := range req.Messages {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			return fmt.Errorf("Request.Messages[%d].Role %q not recognised", i, m.Role)
		}
		if m.Content == "" {
			return fmt.Errorf("Request.Messages[%d].Content is empty", i)
		}
	}
	return nil
}

// --- wire types for Anthropic's Messages API ---

type anthropicBody struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Content []anthropicContent `json:"content"`
	Usage   anthropicUsage     `json:"usage"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func buildAnthropicBody(req Request, model string) ([]byte, error) {
	msgs := make([]anthropicMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = anthropicMessage{Role: string(m.Role), Content: m.Content}
	}
	return json.Marshal(anthropicBody{
		Model:     model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  msgs,
	})
}

func parseAnthropicResponse(body []byte, model string) (*Response, error) {
	var r anthropicResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	var text bytes.Buffer
	for _, c := range r.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	respModel := r.Model
	if respModel == "" {
		respModel = model
	}
	return &Response{
		Text:  text.String(),
		Model: respModel,
		Usage: Usage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
		},
	}, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
