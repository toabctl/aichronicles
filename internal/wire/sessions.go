package wire

// SessionDigest is the wire shape for a single session row as
// returned by /v1/sessions and /v1/sessions/{id}. Maps from the
// store's SessionDigestRow at the handler boundary.
//
// All timestamp / text fields are nullable on the wire because
// mid-flight sessions can have missing ended_at, never-prompted
// sessions can have missing first_prompt, and sessions without a
// summary yet have missing latest_summary. Encode null vs "value"
// rather than collapsing both to "" so callers can distinguish.
type SessionDigest struct {
	ID            string  `json:"id"`
	StartedAtMs   *int64  `json:"started_at_ms,omitempty"`
	EndedAtMs     *int64  `json:"ended_at_ms,omitempty"`
	Cwd           *string `json:"cwd,omitempty"`
	FirstPrompt   *string `json:"first_prompt,omitempty"`
	LatestSummary *string `json:"latest_summary,omitempty"`
	// EventCount is populated by GET /v1/sessions only; the
	// per-session detail endpoint leaves it 0. omitempty drops
	// it from the wire when unset.
	EventCount int `json:"event_count,omitempty"`
	// SourceAgent / SourceSessionID identify the upstream agent
	// (claude-code / gemini-cli / …) and its own session id.
	// Populated by the detail endpoint (/v1/sessions/{id}); the
	// list endpoint leaves them empty (omitempty) to keep the
	// response slim. Consumed by the web's Resume button to
	// render `claude --resume <id>` / `gemini --resume <id>`.
	SourceAgent     string `json:"source_agent,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty"`
}

// SessionListRequest is the query-shape for GET /v1/sessions.
//
// SinceMs is an inclusive cutoff against the session's effective
// timestamp (ended_at_ms when set, else started_at_ms). Sessions
// older than SinceMs are excluded. Zero means "no cutoff" (server
// applies a generous default — 30 days).
type SessionListRequest struct {
	SinceMs int64 `json:"since_ms,omitempty"`
	// Cwd narrows to sessions whose cwd matches exactly. Empty
	// means "any cwd" (the default).
	Cwd   string `json:"cwd,omitempty"`
	Limit int    `json:"limit,omitempty"`

	// Facet filters — every field empty means "no filter for this
	// dimension". Used by the web's session-list page; the MCP
	// list_sessions tool only sets Cwd / SinceMs / Limit.

	// SourceAgent narrows to sessions whose source_agent matches
	// exactly (e.g. "claude-code", "gemini-cli").
	SourceAgent string `json:"source_agent,omitempty"`
	// Project narrows to sessions whose cwd is `project` or a
	// descendant (LIKE `project/%`). Useful for "show me every
	// session in /work/foo/" without an exact-cwd filter.
	Project string `json:"project,omitempty"`
	// ToolName narrows to sessions that have at least one event
	// with this tool_name. EXISTS in events.
	ToolName string `json:"tool_name,omitempty"`
	// SkillName narrows to sessions that have a skill_load
	// extraction for this skill. EXISTS in extractions.
	SkillName string `json:"skill_name,omitempty"`
	// FilePathSubstring narrows to sessions whose file_path
	// extractions LIKE %sub% (case-sensitive).
	FilePathSubstring string `json:"file_path_substring,omitempty"`
	// WithFailures narrows to sessions with at least one
	// tool_failure event.
	WithFailures bool `json:"with_failures,omitempty"`
	// WithoutSummary narrows to sessions that have NO cached
	// summary llm_output. Pairs with the web's "(no summary yet)"
	// muted row state for queueing summarize runs.
	WithoutSummary bool `json:"without_summary,omitempty"`
}

// SessionListResponse is the body shape for GET /v1/sessions.
type SessionListResponse struct {
	Sessions []SessionDigest `json:"sessions"`
}

// SourceAgentsResponse is the body for GET /v1/sessions/source-agents.
// Returns the distinct sessions.source_agent values, alphabetised —
// used by the web's facet picker to render an agent select.
type SourceAgentsResponse struct {
	SourceAgents []string `json:"source_agents"`
}

// CandidateSession is the wire shape for a related-session row
// returned by /v1/sessions/{id}/related. The "related" relation
// is currently same-cwd same-anchor temporal locality (see
// store.LoadCandidatePriorSessions for the criterion).
type CandidateSession struct {
	ID          string `json:"id"`
	Cwd         string `json:"cwd"`
	StartedAtMs int64  `json:"started_at_ms"`
	EndedAtMs   int64  `json:"ended_at_ms"`
	Topic       string `json:"topic"`
}

// CandidateSessionListResponse is the body shape for
// GET /v1/sessions/{id}/related.
type CandidateSessionListResponse struct {
	Candidates []CandidateSession `json:"candidates"`
}

// ResolveSessionResponse is the body shape for
// GET /v1/sessions/resolve?prefix=. Returns the canonical
// session id matching the supplied prefix; the endpoint replies
// 404 on no match and 409 on ambiguous prefix.
type ResolveSessionResponse struct {
	ID string `json:"id"`
}
