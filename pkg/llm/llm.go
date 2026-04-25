// Package llm wraps outbound calls to hosted language models. All
// callers in Block B (summarize, reflect, propose) go through the
// Client interface here, which makes it straightforward to
//
//   - swap providers without touching the feature code
//   - inject a fake Client in tests without spinning up HTTP
//   - funnel every outbound prompt through one logging/metering choke
//     point when we add one
//
// Block A's redact.Outbound is the last line of defense before a
// prompt leaves the process. That shim is invoked by the prompt
// builders in pkg/llm/prompts, NOT here — this package is the
// dumb transport layer. Keeping the egress scrub at prompt-build time
// lets callers choose to abort (via redact.MustClean) if a sensitive
// pattern appears, before anything crosses the network.
//
// Reuse: this package is provider-neutral and aichronicles-agnostic.
// Any Go program needing structured-output LLM calls (forced tool
// use, schema-validated JSON inputs, provider-portable cache keys)
// can import it directly. aichronicles is a work in progress, so
// the public surface may still change.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies the speaker of a Message in a multi-turn Request.
// Values mirror Anthropic's Messages API: user messages from the
// client, assistant messages for prior model output when we want the
// model to continue.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in a conversation passed to the LLM. Content is
// plain text; multimodal content is out of scope for now.
type Message struct {
	Role    Role
	Content string
}

// Request is the provider-neutral shape every Block B feature builds
// and hands to Client.Complete. It intentionally does not expose
// provider-specific knobs (temperature, top_p, stop sequences) — add
// them only when a real caller needs them.
type Request struct {
	// Model is the provider-specific model identifier. The client is
	// responsible for mapping it; this package does not enforce a
	// hard-coded list.
	Model string

	// System, when non-empty, is the system prompt. Zero value means
	// "no system prompt" — do NOT invent one.
	System string

	// Messages is the user/assistant turn list, oldest first. Must
	// start with a user turn; the client enforces.
	Messages []Message

	// MaxTokens caps the model's output length. Must be positive.
	// Callers should size this to their expected summary length —
	// oversizing wastes budget if the provider bills per max, not
	// per generated.
	MaxTokens int

	// Tools, when non-empty, advertises structured-output tools the
	// model may call. Block B features (summarize/reflect/propose)
	// use a single forced tool per call so the reply arrives as
	// validated JSON rather than free-form prose.
	Tools []Tool

	// ForceTool, when non-empty, names the tool the model MUST call.
	// Ignored when Tools is empty. The provider translates this into
	// tool_choice={"type":"tool","name":ForceTool}.
	ForceTool string
}

// Tool declares one structured-output function the model can call.
// InputSchema is the caller's JSON Schema (as raw bytes so the
// provider wire can forward it without re-marshaling).
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Response is the model's reply plus bookkeeping the caller will
// usually want to persist.
type Response struct {
	// Text is the assembled reply. For providers that stream multiple
	// content blocks we concatenate them here so callers get one field.
	// Empty when the model responded exclusively via tool_use.
	Text string

	// Model echoes the provider's report of which model served the
	// request. Often identical to Request.Model, but may differ on
	// providers that alias names to versioned backends.
	Model string

	// Usage records the token counts the provider reports back.
	// Zero values mean "provider did not supply".
	Usage Usage

	// ToolUses is the list of structured-output calls the model made.
	// Populated only when Request.Tools was non-empty. Callers with
	// ForceTool set expect exactly one entry.
	ToolUses []ToolUse
}

// ToolUse is one invocation of a structured-output tool. Input is the
// raw JSON the model produced, suitable for Unmarshal into the
// caller's typed result.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Usage is the token accounting block returned by the provider.
// Kept provider-neutral; individual providers map their response
// fields into these.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Client is the single interface every Block B feature talks to. One
// Complete call per feature invocation; streaming is a non-goal for
// summarize/reflect/propose (the user never sees the raw token stream,
// we just persist the full text when it arrives).
type Client interface {
	Complete(ctx context.Context, req Request) (*Response, error)
}
