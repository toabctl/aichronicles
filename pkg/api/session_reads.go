package api

// SessionEvent is the wire shape for one stored event row as
// returned by GET /v1/sessions/{id}/events. Mirrors events.EventView
// with nullable columns projected to *T pointers. The richer
// per-session view contrasts with api.Event, the slimmer cross-session
// listing shape returned by /v1/events.
type SessionEvent struct {
	EventID      string  `json:"event_id"`
	Kind         string  `json:"kind"`
	Role         *string `json:"role,omitempty"`
	ContentText  *string `json:"content_text,omitempty"`
	TsSourceMs   int64   `json:"ts_source_ms"`
	ToolName     *string `json:"tool_name,omitempty"`
	SubagentID   *string `json:"subagent_id,omitempty"`
	SubagentType *string `json:"subagent_type,omitempty"`
	Cwd          *string `json:"cwd,omitempty"`
}

// SessionEventsResponse is the body for /v1/sessions/{id}/events.
type SessionEventsResponse struct {
	Events []SessionEvent `json:"events"`
}

// Extraction is the wire shape for one extractions row, returned
// by GET /v1/sessions/{id}/extractions?kind=.
type Extraction struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// SessionExtractionsResponse is the body for /v1/sessions/{id}/extractions.
type SessionExtractionsResponse struct {
	Extractions []Extraction `json:"extractions"`
}

// SessionDigest already exists in sessions.go (the public-facing
// wire shape returned by /v1/sessions). The richer
// SessionDigestRow used by reflect/propose carries additional
// fields — Topic, FirstPrompt, EventCount — which the existing
// SessionDigest already covers via LatestSummary + FirstPrompt +
// EventCount. Reused as-is.

// SessionDigestsResponse is the body for /v1/sessions/digests —
// the LoadRecentSessionDigests path used by reflect/propose to
// build a window of session summaries with full enrichment fields.
type SessionDigestsResponse struct {
	Digests []SessionDigest `json:"digests"`
}

// SessionOutcome is the wire shape for one session_outcomes row,
// returned by GET /v1/sessions/{id}/outcome. The endpoint reads-
// or-backfills, mirroring store.EnsureSessionOutcome — a
// previously-uncomputed session lands a fresh row on the first
// read, and subsequent reads hit the cached value.
//
// Mirrors store.SessionOutcome 1:1; nullable LastEventKind is
// projected to *string.
type SessionOutcome struct {
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
	// Outcome is the label string ("success_likely", "failure_likely",
	// "mixed", "unknown"). Mirrors store.OutcomeLabel.
	Outcome string `json:"outcome"`
}
