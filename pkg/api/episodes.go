package api

// Episode is the wire shape for one stored episode — a bounded,
// contextually-coherent run of events within a session. Maps from
// internal/events.Episode at the handler boundary; the wire form uses
// *string for nullable Cwd so JSON consumers do not need a custom
// helper.
type Episode struct {
	ID            int64   `json:"id"`
	SessionID     string  `json:"session_id"`
	Ordinal       int     `json:"ordinal"`
	StartedAtMs   int64   `json:"started_at_ms"`
	EndedAtMs     int64   `json:"ended_at_ms"`
	Cwd           *string `json:"cwd,omitempty"`
	IntentSummary string  `json:"intent_summary"`
	EventCount    int     `json:"event_count"`
	FirstEventID  string  `json:"first_event_id"`
}

// EpisodeListRequest is the query-shape for GET /v1/episodes.
//
// All filters are optional and AND'd together. SessionID empty
// means "any session"; Cwd empty means "any cwd"; QueryContains
// empty means "any intent_summary" (not a regex — substring,
// case-insensitive). SinceMs is an inclusive ended_at_ms cutoff.
type EpisodeListRequest struct {
	SessionID     string `json:"session_id,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	QueryContains string `json:"query_contains,omitempty"`
	SinceMs       int64  `json:"since_ms,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

// EpisodeListResponse is the body shape for GET /v1/episodes.
type EpisodeListResponse struct {
	Episodes []Episode `json:"episodes"`
}
