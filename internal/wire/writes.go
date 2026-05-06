package wire

// SaveLLMOutputRequest is the body shape for POST /v1/llm-outputs.
// SessionID is optional — bare cache rows (e.g. propose-merge
// outputs) don't anchor to a specific session.
type SaveLLMOutputRequest struct {
	SessionID    *string `json:"session_id,omitempty"`
	Kind         string  `json:"kind"`
	Model        string  `json:"model"`
	PromptHash   string  `json:"prompt_hash"`
	InputTokens  *int64  `json:"input_tokens,omitempty"`
	OutputTokens *int64  `json:"output_tokens,omitempty"`
	Body         string  `json:"body"`
	CreatedAtMs  int64   `json:"created_at_ms"`
}

// SaveLLMOutputResponse echoes the assigned id and whether this
// call inserted a new row (vs hit an existing (kind,prompt_hash)
// — common when the caller idempotently re-runs a prompt).
type SaveLLMOutputResponse struct {
	ID       int64 `json:"id"`
	Inserted bool  `json:"inserted"`
}

// SaveEpisodesRequest is the body for POST /v1/episodes. The
// store replaces every episode row for SessionID atomically; an
// empty Episodes slice clears the session's episodes.
type SaveEpisodesRequest struct {
	SessionID string    `json:"session_id"`
	Episodes  []Episode `json:"episodes"`
}

// SaveEpisodesResponse reports the number of rows written.
type SaveEpisodesResponse struct {
	Saved int `json:"saved"`
}

// SaveSemanticFactRequest is the body for POST /v1/facts.
type SaveSemanticFactRequest struct {
	SourceLLMOutputID int64   `json:"source_llm_output_id"`
	Subject           string  `json:"subject"`
	Predicate         string  `json:"predicate"`
	Object            string  `json:"object"`
	Confidence        float64 `json:"confidence"`
	EvidenceSessionID *string `json:"evidence_session_id,omitempty"`
	EvidenceQuote     *string `json:"evidence_quote,omitempty"`
	AssertedAtMs      int64   `json:"asserted_at_ms"`
}

// SaveSemanticFactResponse echoes the fact id (insert or upsert).
type SaveSemanticFactResponse struct {
	ID int64 `json:"id"`
}

// SaveSessionOutcomeRequest is the body for POST /v1/session-outcomes.
type SaveSessionOutcomeRequest struct {
	SessionID         string  `json:"session_id"`
	ComputedAtMs      int64   `json:"computed_at_ms"`
	UserPromptCount   int     `json:"user_prompt_count"`
	ToolUseCount      int     `json:"tool_use_count"`
	ToolFailureCount  int     `json:"tool_failure_count"`
	ErrorCount        int     `json:"error_count"`
	CompactCount      int     `json:"compact_count"`
	GitUndoCount      int     `json:"git_undo_count"`
	PromptRepeatCount int     `json:"prompt_repeat_count"`
	LastEventKind     *string `json:"last_event_kind,omitempty"`
	Outcome           string  `json:"outcome"`
}

// SessionLink is the wire shape for one outgoing session link.
type SessionLink struct {
	ToSessionID string `json:"to_session_id"`
	Kind        string `json:"kind"`
	Rationale   string `json:"rationale"`
	CreatedAtMs int64  `json:"created_at_ms"`
}

// SaveSessionLinksRequest is the body for POST /v1/session-links.
// The store replaces every link from FromSessionID atomically.
type SaveSessionLinksRequest struct {
	FromSessionID string        `json:"from_session_id"`
	Links         []SessionLink `json:"links"`
}
