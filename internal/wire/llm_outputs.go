package wire

// LLMOutput is the wire shape for one llm_outputs cache row,
// returned by /v1/llm-outputs and /v1/summaries. Maps from
// store.LLMOutput at the handler boundary.
type LLMOutput struct {
	ID           int64   `json:"id"`
	SessionID    *string `json:"session_id,omitempty"`
	Kind         string  `json:"kind"`
	Model        string  `json:"model"`
	PromptHash   string  `json:"prompt_hash"`
	InputTokens  *int64  `json:"input_tokens,omitempty"`
	OutputTokens *int64  `json:"output_tokens,omitempty"`
	Body         string  `json:"body"`
	CreatedAtMs  int64   `json:"created_at_ms"`
}
