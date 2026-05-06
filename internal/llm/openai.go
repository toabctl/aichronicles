package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	openaisdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"

	"github.com/toabctl/aichronicles/internal/redact"
)

// OpenAIAPIKeyEnv is the environment variable read by FromEnvOpenAI
// when a config doesn't supply an api_key_command. Mirrors APIKeyEnv
// for Anthropic.
const OpenAIAPIKeyEnv = "OPENAI_API_KEY"

// DefaultOpenAIModel is the model OpenAI requests fall back to when
// Request.Model is empty. gpt-4o-mini is widely available, supports
// tool use with strict schema, and keeps token costs low for the
// kinds of calls Block B makes.
const DefaultOpenAIModel = "gpt-4o-mini"

// OpenAI is a Client backed by github.com/openai/openai-go. Same
// shape as Anthropic so the two adapters slot interchangeably into
// FromConfig. Construct via openAIFromConfig or by literal for
// tests; the SDK client is built once per instance via sdkOnce.
type OpenAI struct {
	APIKey   string
	Endpoint string       // overridable for tests
	HTTP     *http.Client // overridable for tests

	// MaxRetries: 0 → DefaultMaxRetries; negative → no retries.
	MaxRetries int

	sdkOnce   sync.Once
	sdkClient openaisdk.Client
}

// FromEnvOpenAI returns a production OpenAI client built from
// $OPENAI_API_KEY. Errors when the env var is missing.
func FromEnvOpenAI() (Client, error) {
	key := os.Getenv(OpenAIAPIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("llm: %s not set", OpenAIAPIKeyEnv)
	}
	return NewOpenAI(key), nil
}

// FromEnvOrCommandOpenAI mirrors FromEnvOrCommand for OpenAI: env
// first, then the optional shell command. Same execution rules
// (10s timeout, stderr discarded, empty output rejected).
func FromEnvOrCommandOpenAI(ctx context.Context, command string) (Client, error) {
	if key := os.Getenv(OpenAIAPIKeyEnv); key != "" {
		return NewOpenAI(key), nil
	}
	if command == "" {
		return nil, fmt.Errorf("llm: %s not set and no api_key_command configured", OpenAIAPIKeyEnv)
	}
	key, err := runKeyCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	return NewOpenAI(key), nil
}

// NewOpenAI returns an OpenAI client ready for production. Empty key
// is rejected at call time.
func NewOpenAI(apiKey string) *OpenAI {
	return &OpenAI{APIKey: apiKey}
}

// ensureSDK lazily constructs the SDK client into o.sdkClient. Same
// pattern as Anthropic.ensureSDK; storing on the struct keeps the
// pointer-receiver methods callable.
func (o *OpenAI) ensureSDK() {
	o.sdkOnce.Do(func() {
		var opts []option.RequestOption
		opts = append(opts, option.WithAPIKey(o.APIKey))
		if o.Endpoint != "" {
			opts = append(opts, option.WithBaseURL(o.Endpoint))
		}
		if o.HTTP != nil {
			opts = append(opts, option.WithHTTPClient(o.HTTP))
		}
		retries := o.MaxRetries
		if retries == 0 {
			retries = DefaultMaxRetries
		}
		if retries < 0 {
			retries = 0
		}
		opts = append(opts, option.WithMaxRetries(retries))
		o.sdkClient = openaisdk.NewClient(opts...)
	})
}

// Complete issues a Chat Completions call via the SDK and translates
// the reply into the provider-neutral Response. Tool use maps to
// OpenAI's function-calling: each Tool becomes a function-typed tool,
// ForceTool maps to tool_choice={"type":"function","function":{...}},
// and `strict: true` is set on every function so OpenAI server-side
// validates the schema. Token usage is mapped from CompletionUsage.
//
// Errors from the upstream pass through redact.Outbound before being
// returned, so a 401 echoing the API key never lands in a log line.
func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	if o.APIKey == "" {
		return nil, errors.New("openai: API key not set (expected in OPENAI_API_KEY)")
	}
	if err := validateRequest(req); err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}

	model := req.Model
	if model == "" {
		model = DefaultOpenAIModel
	}

	params, err := buildOpenAIParams(req, model)
	if err != nil {
		return nil, err
	}

	o.ensureSDK()
	resp, err := o.sdkClient.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, scrubOpenAIError(err)
	}
	return convertOpenAIResponse(resp, model), nil
}

// buildOpenAIParams maps our provider-neutral Request to the SDK's
// ChatCompletionNewParams. The system prompt rides as the first
// message (OpenAI has no explicit `system` field on chat completions
// — it's just a message with role: system).
func buildOpenAIParams(req Request, model string) (openaisdk.ChatCompletionNewParams, error) {
	msgs := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openaisdk.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			msgs = append(msgs, openaisdk.UserMessage(m.Content))
		case RoleAssistant:
			msgs = append(msgs, openaisdk.AssistantMessage(m.Content))
		default:
			return openaisdk.ChatCompletionNewParams{}, fmt.Errorf("openai: unsupported role %q", m.Role)
		}
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:               shared.ChatModel(model),
		Messages:            msgs,
		MaxCompletionTokens: param.NewOpt(int64(req.MaxTokens)),
	}

	if len(req.Tools) > 0 {
		tools := make([]openaisdk.ChatCompletionToolParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema, err := toOpenAIFunctionParameters(t.InputSchema)
			if err != nil {
				return openaisdk.ChatCompletionNewParams{}, fmt.Errorf("openai: tool %q schema: %w", t.Name, err)
			}
			fn := shared.FunctionDefinitionParam{
				Name:       t.Name,
				Parameters: schema,
				// Strict mode pins schema validation server-side, which
				// matches Anthropic's default behavior on tool inputs.
				// We always want this — we built the schema ourselves
				// and we want guaranteed compliance.
				Strict: param.NewOpt(true),
			}
			if t.Description != "" {
				fn.Description = param.NewOpt(t.Description)
			}
			tools = append(tools, openaisdk.ChatCompletionToolParam{Function: fn})
		}
		params.Tools = tools
	}

	if req.ForceTool != "" {
		params.ToolChoice = openaisdk.ChatCompletionToolChoiceOptionParamOfChatCompletionNamedToolChoice(
			openaisdk.ChatCompletionNamedToolChoiceFunctionParam{Name: req.ForceTool},
		)
	}

	return params, nil
}

// toOpenAIFunctionParameters decodes our raw json.RawMessage tool
// schema into the SDK's FunctionParameters (which is just a
// map[string]any). The SDK forwards whatever shape we pass, so any
// JSON Schema feature we use round-trips intact.
func toOpenAIFunctionParameters(raw json.RawMessage) (shared.FunctionParameters, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m shared.FunctionParameters
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// convertOpenAIResponse translates an SDK *ChatCompletion into our
// provider-neutral Response. We always read the first choice (Block
// B never sets n>1), concatenating .Content as Text and iterating
// .ToolCalls into ToolUses. Function arguments arrive as a JSON
// string; we surface them as RawMessage so callers can Unmarshal
// straight into their typed result.
func convertOpenAIResponse(resp *openaisdk.ChatCompletion, requestedModel string) *Response {
	if resp == nil || len(resp.Choices) == 0 {
		return &Response{Model: requestedModel}
	}
	msg := resp.Choices[0].Message
	var toolUses []ToolUse
	for _, tc := range msg.ToolCalls {
		// Arguments is a JSON-encoded string (per OpenAI API). Wrap
		// it as RawMessage; downstream Unmarshal does the actual
		// parsing into the caller's typed result.
		toolUses = append(toolUses, ToolUse{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	respModel := string(resp.Model)
	if respModel == "" {
		respModel = requestedModel
	}
	return &Response{
		Text:     msg.Content,
		Model:    respModel,
		ToolUses: toolUses,
		Usage: Usage{
			InputTokens:  int(resp.Usage.PromptTokens),
			OutputTokens: int(resp.Usage.CompletionTokens),
		},
	}
}

// scrubOpenAIError mirrors scrubAnthropicError: the SDK's error
// message embeds the raw upstream response body verbatim, so we
// always run it through redact.Outbound before returning.
//
// Order matters: redact first, truncate second. Truncating before
// redacting could split a credential straddling the byte boundary,
// half of which would then escape the regex match and reach the
// caller.
func scrubOpenAIError(err error) error {
	if err == nil {
		return nil
	}
	scrubbed, _ := redact.Outbound(err.Error())
	if len(scrubbed) > 1024 {
		scrubbed = scrubbed[:1024] + "…"
	}
	return fmt.Errorf("openai: %s", scrubbed)
}
