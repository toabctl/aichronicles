package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/redact"
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

// DefaultMaxRetries is the number of retry attempts Complete will make
// on top of the initial request when the provider returns a retryable
// status (429 or 5xx) or the network call fails. Total attempts =
// initial + DefaultMaxRetries. A budget of 3 survives a brief 429 or
// transient upstream hiccup without turning a flaky API into hangs.
const DefaultMaxRetries = 3

// defaultRetryBaseDelay is the first-retry wait before jitter. Each
// subsequent retry doubles until defaultRetryMaxDelay. A server-sent
// Retry-After (seconds or HTTP-date) overrides this entirely, capped
// at defaultRetryMaxDelay so a hostile upstream can't pin us.
const (
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 10 * time.Second
)

// Anthropic is a Client that talks to the Anthropic Messages API.
// Safe for concurrent use: http.Client is goroutine-safe and this
// struct holds no other state that mutates after construction.
type Anthropic struct {
	APIKey   string
	Endpoint string       // overridable for tests; empty means AnthropicEndpoint
	HTTP     *http.Client // optional; nil means a sensible default

	// MaxRetries overrides DefaultMaxRetries. Zero uses the default;
	// set negative to disable retries entirely (useful for tests that
	// want to assert a single attempt).
	MaxRetries int

	// RetryBaseDelay overrides defaultRetryBaseDelay. Zero uses the
	// default. Tests set this small (e.g., 1ms) so the retry loop
	// does not wait real wall-clock time.
	RetryBaseDelay time.Duration
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

// keyCommandTimeout caps how long we'll wait for the user's shell
// command to produce the API key. Keyrings usually answer in ms; a
// hung `secret-tool` waiting on a locked keyring should fail fast so
// the user sees an error rather than an apparently-hung CLI.
const keyCommandTimeout = 10 * time.Second

// FromEnvOrCommand returns a Client whose key comes from
// $ANTHROPIC_API_KEY, or — when that is empty — from the stdout of
// `/bin/sh -c <command>`. An empty command combined with an empty
// env yields the same "not set" error FromEnv does.
//
// Trailing whitespace is stripped from the command output. Empty
// output is rejected — a command that fails to produce a key should
// error visibly, not hand us an empty string that the API will
// 401 on later.
//
// Stderr from the command is discarded. This is deliberate: some
// keyring tools write informational prompts to stderr, and we do
// not want to risk echoing a partial key there either. The user can
// debug their command outside aichronicles if it misbehaves.
func FromEnvOrCommand(ctx context.Context, command string) (Client, error) {
	if key := os.Getenv(APIKeyEnv); key != "" {
		return NewAnthropic(key), nil
	}
	if command == "" {
		return nil, fmt.Errorf("llm: %s not set and no api_key_command configured", APIKeyEnv)
	}
	key, err := runKeyCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	return NewAnthropic(key), nil
}

// runKeyCommand executes command via `/bin/sh -c` and returns the
// trimmed stdout. Exported only via FromEnvOrCommand; direct callers
// should go through that so the env-first shortcut applies.
func runKeyCommand(parent context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, keyCommandTimeout)
	defer cancel()

	// #nosec G204 — command is user-supplied via their own 0600
	// config file. The mode-check in config.LoadFrom is the trust
	// boundary; beyond that we treat the command like any other
	// user-chosen shell command.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("llm: api_key_command timed out after %v", keyCommandTimeout)
		}
		return "", fmt.Errorf("llm: api_key_command failed: %w", err)
	}
	key := strings.TrimRight(string(out), " \t\r\n")
	if key == "" {
		return "", fmt.Errorf("llm: api_key_command produced empty output")
	}
	return key, nil
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

// Complete issues a Messages API call and returns the reply text plus
// token usage. Retries on 429 and 5xx using exponential backoff with
// jitter; a server-sent Retry-After header (seconds or HTTP-date)
// overrides the computed delay. Network errors retry the same way.
// The ctx deadline is honored both during I/O and during backoff —
// the loop aborts as soon as ctx is done.
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

	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	maxRetries := a.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := a.doOnce(ctx, client, endpoint, body)
		switch {
		case err == nil && resp != nil && isRetryableStatus(resp.status):
			scrubbed, _ := redact.Outbound(truncate(resp.body, 1024))
			lastErr = fmt.Errorf("anthropic: status %d: %s", resp.status, scrubbed)
			if attempt == maxRetries {
				return nil, lastErr
			}
			delay := retryDelay(resp.retryAfter, attempt, a.RetryBaseDelay)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			continue
		case err == nil && resp != nil && (resp.status < 200 || resp.status >= 300):
			scrubbed, _ := redact.Outbound(truncate(resp.body, 1024))
			return nil, fmt.Errorf("anthropic: status %d: %s", resp.status, scrubbed)
		case err == nil && resp != nil:
			return parseAnthropicResponse(resp.body, model)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return nil, err
		default:
			// Transport-level failure (DNS, connection reset, TLS…)
			// — treat as retryable.
			lastErr = err
			if attempt == maxRetries {
				return nil, lastErr
			}
			delay := retryDelay("", attempt, a.RetryBaseDelay)
			if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
		}
	}
	// Exhausted retries.
	return nil, lastErr
}

// attemptResult is the successful-I/O shape returned by doOnce: the
// status, the fully-read body, and the raw Retry-After header if set.
// We return the body instead of an open reader so the retry loop can
// inspect status + body after the connection has closed.
type attemptResult struct {
	status     int
	body       []byte
	retryAfter string
}

// doOnce performs exactly one HTTP attempt. Network / request-build
// errors come back as the second return. A non-2xx status is NOT an
// error here — the caller decides whether to retry based on the
// status code.
func (a *Anthropic) doOnce(ctx context.Context, client *http.Client, endpoint string, body []byte) (*attemptResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", AnthropicAPIVersion)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("anthropic: read response: %w", err)
	}
	return &attemptResult{
		status:     resp.StatusCode,
		body:       respBody,
		retryAfter: resp.Header.Get("Retry-After"),
	}, nil
}

// isRetryableStatus reports whether a response status code justifies a
// retry. 429 is the rate-limit signal; any 5xx is "server's problem,
// may clear up". 4xx other than 429 is a client bug — retrying won't
// help, so we don't.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// retryDelay picks the next backoff duration. If the server sent a
// Retry-After we honor it (capped at defaultRetryMaxDelay so a hostile
// upstream can't pin us). Otherwise we use exponential backoff with
// ±25% jitter so a swarm of clients doesn't synchronize on retry.
func retryDelay(retryAfter string, attempt int, base time.Duration) time.Duration {
	if d, ok := parseRetryAfter(retryAfter, time.Now()); ok {
		if d > defaultRetryMaxDelay {
			d = defaultRetryMaxDelay
		}
		if d < 0 {
			d = 0
		}
		return d
	}
	if base <= 0 {
		base = defaultRetryBaseDelay
	}
	// Exponential: base, base*2, base*4, …
	shift := attempt
	if shift > 10 {
		shift = 10 // guard against absurd attempt counts
	}
	d := base << shift
	if d > defaultRetryMaxDelay {
		d = defaultRetryMaxDelay
	}
	// ±25% jitter — rand.Float64() is fine here, we don't need crypto.
	jitter := 1 + (rand.Float64()*0.5 - 0.25)
	return time.Duration(float64(d) * jitter)
}

// parseRetryAfter accepts both RFC 7231 forms: an integer number of
// seconds, or an HTTP-date. `now` is injectable so the HTTP-date branch
// is deterministically testable.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return t.Sub(now), true
	}
	return 0, false
}

// sleepCtx waits for d or ctx.Done, whichever comes first. A
// non-positive d returns immediately (subject to ctx).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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

// anthropicBody is the request payload. `System` uses the blocks form
// (not the legacy top-level string) so we can attach cache_control
// for prompt caching — Block B's system prompts are hardcoded constants
// reused across every summarize/reflect/propose call, so the input-
// token cost drops by ~90% on cache hits. The hash in prompts.go
// deliberately keys on the system string (not the wire block form)
// so the cache behavior is transparent to callers.
type anthropicBody struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	System     []systemBlock        `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// anthropicTool is one entry of the request `tools` array. The
// schema field name is `input_schema` on the wire (not `inputSchema`).
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice forces a specific tool call. Type is always
// "tool" for our callers — "auto" and "any" are unused today.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// systemBlock is one entry in the Messages API `system` array. We only
// ever emit a single text block today; the type exists because
// cache_control attaches at the block level.
type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl = {"type":"ephemeral"} is the Anthropic prompt-cache
// directive. Ephemeral caches live ~5 minutes and are keyed by the
// exact block content. We only attach it to the system block, which
// is the one part of the prompt that is identical across calls.
type cacheControl struct {
	Type string `json:"type"`
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

// anthropicContent models one content block in the response. Text
// blocks populate Text; tool_use blocks populate ID, Name, and Input
// (the model's JSON arguments). Blocks of other types are ignored.
type anthropicContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
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
	var system []systemBlock
	if req.System != "" {
		system = []systemBlock{{
			Type:         "text",
			Text:         req.System,
			CacheControl: &cacheControl{Type: "ephemeral"},
		}}
	}
	var tools []anthropicTool
	if len(req.Tools) > 0 {
		tools = make([]anthropicTool, len(req.Tools))
		for i, t := range req.Tools {
			// anthropicTool has the same fields as Tool plus JSON
			// tags; Go allows this conversion despite the tag
			// difference (spec: field tags are ignored for struct
			// conversions). Keeps us from drifting if either side
			// gains a field.
			tools[i] = anthropicTool(t)
		}
	}
	var toolChoice *anthropicToolChoice
	if req.ForceTool != "" {
		toolChoice = &anthropicToolChoice{Type: "tool", Name: req.ForceTool}
	}
	return json.Marshal(anthropicBody{
		Model:      model,
		MaxTokens:  req.MaxTokens,
		System:     system,
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: toolChoice,
	})
}

func parseAnthropicResponse(body []byte, model string) (*Response, error) {
	var r anthropicResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	var text bytes.Buffer
	var toolUses []ToolUse
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			toolUses = append(toolUses, ToolUse{
				ID:    c.ID,
				Name:  c.Name,
				Input: c.Input,
			})
		}
	}
	respModel := r.Model
	if respModel == "" {
		respModel = model
	}
	return &Response{
		Text:     text.String(),
		Model:    respModel,
		ToolUses: toolUses,
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
