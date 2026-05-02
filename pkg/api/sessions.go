package api

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
}

// SessionListRequest is the query-shape for GET /v1/sessions.
//
// SinceMs is an inclusive cutoff against the session's effective
// timestamp (ended_at_ms when set, else started_at_ms). Sessions
// older than SinceMs are excluded. Zero means "no cutoff" (server
// applies a generous default — 30 days).
type SessionListRequest struct {
	SinceMs int64 `json:"since_ms,omitempty"`
	Limit   int   `json:"limit,omitempty"`
}

// SessionListResponse is the body shape for GET /v1/sessions.
type SessionListResponse struct {
	Sessions []SessionDigest `json:"sessions"`
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
