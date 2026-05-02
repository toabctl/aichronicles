package api

// SubagentSpan is the wire shape for one /v1/subagents row.
// Maps from store.SubagentSpan. SubagentType is nullable on the
// wire because some subagents lack a typed designation.
type SubagentSpan struct {
	SessionID    string  `json:"session_id"`
	SubagentID   string  `json:"subagent_id"`
	SubagentType *string `json:"subagent_type,omitempty"`
	StartedAtMs  int64   `json:"started_at_ms"`
	EndedAtMs    int64   `json:"ended_at_ms"`
	EventCount   int     `json:"event_count"`
}

// SubagentsResponse is the body for /v1/subagents.
type SubagentsResponse struct {
	Spans []SubagentSpan `json:"spans"`
}
