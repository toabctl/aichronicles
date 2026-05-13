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

// SummariesBatchResponse is the body for GET /v1/summaries/batch.
// Keyed by session_id; sessions without a cached summary are simply
// absent from the map (no "null" sentinel). Used by the web's
// session-list page to enrich N rows with their latest summary in a
// single round-trip, avoiding N HTTP calls.
type SummariesBatchResponse struct {
	Summaries map[string]LLMOutput `json:"summaries"`
}

// LLMOutputsListResponse is the body for GET /v1/llm-outputs/list
// and GET /v1/sessions/{id}/llm-outputs. Wrapped in an object (not a
// bare array) so future fields like pagination cursors don't break
// existing clients.
type LLMOutputsListResponse struct {
	Outputs []LLMOutput `json:"outputs"`
}

// LLMOutputLastCreatedAtResponse is the body for
// GET /v1/llm-outputs/last-created-at?kind=. LastCreatedAtMs is 0
// when no rows of that kind exist.
type LLMOutputLastCreatedAtResponse struct {
	LastCreatedAtMs int64 `json:"last_created_at_ms"`
}

// LLMOutputExistsResponse is the body for
// GET /v1/llm-outputs/exists?session_id=&kind=.
type LLMOutputExistsResponse struct {
	Exists bool `json:"exists"`
}
