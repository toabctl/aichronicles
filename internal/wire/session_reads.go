package wire

import "slices"

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

// SessionLinkKind enumerates the four typed inter-session relationships
// the summarize prompt is allowed to emit. The list is closed — the
// summarize tool schema and the SQL migration's CHECK constraint both
// reject anything else. Lives in wire/ because it's protocol-level
// vocabulary every JSON body and every UI listing consumes; store/
// re-exports these via type aliases for backwards compat.
//
// Semantics:
//
//   - builds_on:           this session continues / extends the prior session's work.
//   - repeats_failure_of:  this session hit the same wall the prior session hit.
//   - supersedes:          this session's outcome replaces the prior session's outcome
//     (e.g. we redid the migration the right way).
//   - related:             topical overlap, but no causal claim.
const (
	SessionLinkBuildsOn         = "builds_on"
	SessionLinkRepeatsFailureOf = "repeats_failure_of"
	SessionLinkSupersedes       = "supersedes"
	SessionLinkRelated          = "related"
)

// SessionLinkKinds is the canonical ordered list, also the order the UI
// renders. "related" last so it doesn't visually crowd out the more
// meaningful causal kinds.
var SessionLinkKinds = []string{
	SessionLinkBuildsOn,
	SessionLinkRepeatsFailureOf,
	SessionLinkSupersedes,
	SessionLinkRelated,
}

// IsValidSessionLinkKind reports whether k is one of the four
// canonical kinds. The SQL migration's CHECK clause and the
// summarize prompt's allowed-set both gate on the same membership
// — keep these in sync if a fifth kind is ever added. Lives here
// (next to the constants it validates) so non-store callers can
// reach the predicate without pulling internal/store.
func IsValidSessionLinkKind(k string) bool {
	return slices.Contains(SessionLinkKinds, k)
}

// SessionLinkRow is the wire shape for one session_links row returned
// by GET /v1/session-links. Both ends are populated unconditionally
// so the web's outgoing/incoming sidebars consume one shape.
type SessionLinkRow struct {
	FromSessionID string `json:"from_session_id"`
	ToSessionID   string `json:"to_session_id"`
	Kind          string `json:"kind"`
	Rationale     string `json:"rationale"`
	CreatedAtMs   int64  `json:"created_at_ms"`
}

// SessionLinksResponse is the body for GET /v1/session-links?from=X
// or ?to=X.
type SessionLinksResponse struct {
	Links []SessionLinkRow `json:"links"`
}

// SessionStartCwdResponse is the body for GET /v1/sessions/{id}/start-cwd.
// Cwd is nil when the session has no event with a recorded cwd — the
// caller (web's resume button) decides whether to fall back to
// sessions.cwd or hide the affordance entirely. The wire layer
// returns 200-with-null rather than 404 because "no start_cwd" is a
// normal state, not an error.
type SessionStartCwdResponse struct {
	Cwd *string `json:"cwd"`
}

// SessionOutcome lives in session_outcomes.go alongside the
// OutcomeLabel constants — see that file. Documented here so
// readers chasing the GET /v1/sessions/{id}/outcome shape find a
// pointer.
