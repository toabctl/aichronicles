package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/toabctl/aichronicles/internal/redact"
)

// AnthropicEndpoint is the Messages API URL. Exported so tests can
// override it via Anthropic.Endpoint to point at httptest.
const AnthropicEndpoint = "https://api.anthropic.com/v1/messages"

// DefaultAnthropicModel is the identifier Block B features fall back
// to when the user has not specified one. Opus 4.7 is the most
// capable Claude model currently available; aichronicles uses it
// across the board (summarize, reflect, propose, challenge,
// induction, search-summarize) for the best output quality on the
// memory-extraction pipeline. Callers can still override per-request
// via Request.Model — pass --model claude-sonnet-4-6 (or another id)
// on the CLI to trade quality for cost on a specific run.
const DefaultAnthropicModel = "claude-opus-4-7"

// DefaultMaxRetries is the retry budget the SDK is configured with
// when a caller leaves Anthropic.MaxRetries at zero. The SDK's own
// default is 2; we bump to 3 because Block B features (summarize,
// reflect, propose) are interactive and a brief 429 mid-business-day
// shouldn't bubble up to the user. Set Anthropic.MaxRetries < 0 to
// disable retries.
const DefaultMaxRetries = 3

// APIKeyEnv is the environment variable the CLI subcommands (and
// FromEnv) read when wiring up a production Anthropic client.
// Exported so user-facing docs can reference it without hard-coding
// the string.
const APIKeyEnv = "ANTHROPIC_API_KEY"

// Anthropic is a Client backed by the official anthropic-sdk-go.
// Safe for concurrent use: the SDK client is built once via sdkOnce
// and reused across calls. Tests construct an Anthropic{} literal
// and override Endpoint/HTTP to point at httptest; production code
// goes through NewAnthropic / FromEnv / FromEnvOrCommand.
type Anthropic struct {
	// APIKey is the credential header value. Required; the SDK
	// rejects empty.
	APIKey string

	// Endpoint, when non-empty, overrides the SDK's default base
	// URL. Used by tests to point at httptest.NewServer.
	Endpoint string

	// HTTP, when non-nil, replaces the SDK's default *http.Client.
	// Tests use httptest.Server.Client() so test certs validate
	// cleanly; production callers leave this nil.
	HTTP *http.Client

	// MaxRetries overrides DefaultMaxRetries. Zero uses the default;
	// negative disables retries (one attempt only).
	MaxRetries int

	sdkOnce   sync.Once
	sdkClient anthropicsdk.Client
}

// keyCommandTimeout caps how long we'll wait for the user's shell
// command to produce the API key.
const keyCommandTimeout = 10 * time.Second

// FromEnv returns a production Anthropic client built from
// $ANTHROPIC_API_KEY. Errors when the env var is missing.
func FromEnv() (Client, error) {
	key := os.Getenv(APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("llm: %s not set", APIKeyEnv)
	}
	return NewAnthropic(key), nil
}

// FromEnvOrCommand returns a Client whose key comes from
// $ANTHROPIC_API_KEY, or — when that is empty — from the stdout of
// `/bin/sh -c <command>`. Empty env + empty command yields the same
// "not set" error FromEnv does. Stderr from the command is discarded
// so chatty keyring prompts don't corrupt the resolve.
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
// trimmed stdout.
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

// NewAnthropic returns a Client ready to hit production. Empty key
// is rejected at call time, not here, so test fixtures that never
// issue requests can skip wiring one up.
func NewAnthropic(apiKey string) *Anthropic {
	return &Anthropic{APIKey: apiKey}
}

// ensureSDK lazily constructs the SDK client into a.sdkClient. Built
// once per Anthropic instance; subsequent mutations to APIKey/
// Endpoint/HTTP/MaxRetries are NOT picked up — same shape as the
// previous hand-rolled client. Tests construct fresh instances per
// case.
//
// Stored on the struct (rather than returned) so callers can call
// methods with pointer receivers like `a.sdkClient.Messages.New(...)`
// — the SDK's MessageService.New takes a pointer receiver, which
// requires an addressable value (a struct field qualifies; a
// returned value does not).
func (a *Anthropic) ensureSDK() {
	a.sdkOnce.Do(func() {
		var opts []option.RequestOption
		opts = append(opts, option.WithAPIKey(a.APIKey))
		if a.Endpoint != "" {
			opts = append(opts, option.WithBaseURL(a.Endpoint))
		}
		if a.HTTP != nil {
			opts = append(opts, option.WithHTTPClient(a.HTTP))
		}
		retries := a.MaxRetries
		if retries == 0 {
			retries = DefaultMaxRetries
		}
		if retries < 0 {
			retries = 0
		}
		opts = append(opts, option.WithMaxRetries(retries))
		a.sdkClient = anthropicsdk.NewClient(opts...)
	})
}

// Complete issues a Messages API call via the SDK and translates the
// reply into the provider-neutral Response. Retries (429 + 5xx +
// 408 + 409, with Retry-After honored) are handled internally by
// the SDK using the budget configured in sdk(). Errors from the
// upstream are scrubbed through redact.Outbound before being
// returned, so a misconfigured 401 echoing the API key never lands
// in a log line.
func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	if a.APIKey == "" {
		return nil, fmt.Errorf("anthropic: %w (expected in ANTHROPIC_API_KEY)", ErrNoAPIKey)
	}
	if err := validateRequest(req); err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	model := req.Model
	if model == "" {
		model = DefaultAnthropicModel
	}

	params, err := buildAnthropicParams(req, model)
	if err != nil {
		return nil, err
	}

	a.ensureSDK()
	// Tool-forced calls disable SDK retries per-request: the Usage
	// struct on the returned message reflects only the FINAL
	// attempt, so a 429 retried twice silently triples input-token
	// spend while the accounting reports 1×. Equally bad, each
	// retry re-sends the cache-controlled system prefix and pays
	// the cache-miss cost again. The caller (propose / reflect /
	// induce / facts / summary) decides whether to re-issue on
	// transient failure; surfacing the 429 lets them account for
	// the full attempt cost.
	var callOpts []option.RequestOption
	if req.ForceTool != "" {
		callOpts = append(callOpts, option.WithMaxRetries(0))
	}
	msg, err := a.sdkClient.Messages.New(ctx, params, callOpts...)
	if err != nil {
		return nil, scrubAnthropicError(err)
	}

	return convertAnthropicMessage(msg, model), nil
}

// buildAnthropicParams maps our provider-neutral Request into the
// SDK's MessageNewParams. The system prompt rides as a single text
// block with cache_control=ephemeral so Anthropic's prompt cache
// kicks in on repeats — Block B's system prompts are constants, so
// every reuse within a 5-minute window is a cache hit on input
// tokens. Empty Tools / ForceTool fields produce a wire body
// without those keys, which matters for prompt-cache stability.
func buildAnthropicParams(req Request, model string) (anthropicsdk.MessageNewParams, error) {
	msgs := make([]anthropicsdk.MessageParam, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			msgs[i] = anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(m.Content))
		case RoleAssistant:
			msgs[i] = anthropicsdk.NewAssistantMessage(anthropicsdk.NewTextBlock(m.Content))
		default:
			return anthropicsdk.MessageNewParams{}, fmt.Errorf("anthropic: unsupported role %q", m.Role)
		}
	}

	params := anthropicsdk.MessageNewParams{
		Model:     anthropicsdk.Model(model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  msgs,
	}

	if req.System != "" {
		params.System = []anthropicsdk.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropicsdk.NewCacheControlEphemeralParam(),
		}}
	}

	if len(req.Tools) > 0 {
		tools := make([]anthropicsdk.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema, err := toAnthropicToolSchema(t.InputSchema)
			if err != nil {
				return anthropicsdk.MessageNewParams{}, fmt.Errorf("anthropic: tool %q schema: %w", t.Name, err)
			}
			tp := anthropicsdk.ToolParam{
				Name:        t.Name,
				InputSchema: schema,
			}
			if t.Description != "" {
				tp.Description = param.NewOpt(t.Description)
			}
			tools = append(tools, anthropicsdk.ToolUnionParam{OfTool: &tp})
		}
		params.Tools = tools
	}

	if req.ForceTool != "" {
		params.ToolChoice = anthropicsdk.ToolChoiceParamOfTool(req.ForceTool)
	}

	return params, nil
}

// toAnthropicToolSchema translates our raw json.RawMessage tool
// schema into the SDK's typed ToolInputSchemaParam. Top-level
// `properties` and `required` are extracted into named fields;
// anything else (additionalProperties, $defs, etc.) lands in
// ExtraFields so it round-trips. The schema's `type` key is dropped
// because the SDK pins it to "object" — every tool we ship has
// type:"object" anyway.
func toAnthropicToolSchema(raw json.RawMessage) (anthropicsdk.ToolInputSchemaParam, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return anthropicsdk.ToolInputSchemaParam{}, err
	}
	var schema anthropicsdk.ToolInputSchemaParam
	if p, ok := m["properties"]; ok {
		schema.Properties = p
		delete(m, "properties")
	}
	if reqAny, ok := m["required"].([]any); ok {
		for _, r := range reqAny {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
		delete(m, "required")
	}
	delete(m, "type") // SDK constant
	if len(m) > 0 {
		schema.ExtraFields = m
	}
	return schema, nil
}

// convertAnthropicMessage translates the SDK's *Message into our
// provider-neutral Response. Text blocks concatenate (preserving
// order) and tool_use blocks populate Response.ToolUses. Other
// content types (thinking, server_tool_use, etc.) are ignored by
// design — Block B does not negotiate them.
func convertAnthropicMessage(msg *anthropicsdk.Message, requestedModel string) *Response {
	if msg == nil {
		return &Response{Model: requestedModel}
	}
	var text strings.Builder
	var toolUses []ToolUse
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			toolUses = append(toolUses, ToolUse{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}
	respModel := string(msg.Model)
	if respModel == "" {
		respModel = requestedModel
	}
	return &Response{
		Text:       text.String(),
		Model:      respModel,
		ToolUses:   toolUses,
		StopReason: mapAnthropicStopReason(msg.StopReason),
		Usage: Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
	}
}

// mapAnthropicStopReason normalises the SDK's stop_reason string into
// our provider-neutral StopReason. Only max_tokens and tool_use carry
// orchestration meaning; everything else (end_turn, stop_sequence,
// pause_turn, refusal, …) collapses to StopOther.
func mapAnthropicStopReason(r anthropicsdk.StopReason) StopReason {
	switch r {
	case anthropicsdk.StopReasonMaxTokens:
		return StopMaxTokens
	case anthropicsdk.StopReasonToolUse:
		return StopToolUse
	default:
		return StopOther
	}
}

// scrubAnthropicError wraps an SDK error so its message — which
// includes the raw upstream response body for HTTP failures — never
// echoes a leaked API key or other secret.
//
// Order matters: redact first, truncate second. Truncating before
// redacting could split a credential straddling the byte boundary,
// half of which would then escape the regex match and reach the
// caller.
func scrubAnthropicError(err error) error {
	if err == nil {
		return nil
	}
	scrubbed, _ := redact.Outbound(err.Error())
	if len(scrubbed) > 1024 {
		scrubbed = scrubbed[:1024] + "…"
	}
	return fmt.Errorf("anthropic: %s", scrubbed)
}
