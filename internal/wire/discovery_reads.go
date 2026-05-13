package wire

// SessionsMissingSummaryRequest is the query-shape for
// GET /v1/sessions/missing-summary. Mirrors store.SessionFilter
// + a since_ms cutoff + limit. Used by the summaries fill /
// missing CLI to find sessions that need an LLM summary.
type SessionsMissingSummaryRequest struct {
	SinceMs int64  `json:"since_ms,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// SessionsMissingSummaryResponse is the body for
// /v1/sessions/missing-summary. Reuses SessionDigest as the row
// shape — every field needed for the table view (id, ts bounds,
// cwd, first_prompt) is already on it.
type SessionsMissingSummaryResponse struct {
	Sessions []SessionDigest `json:"sessions"`
}

// SessionsNeedingSegmentationRequest is the query-shape for
// GET /v1/sessions/needing-segmentation. Same predicates the
// induction sweep applies before episode segmentation: idle long
// enough that the session has settled, substantial enough to be
// worth segmenting.
type SessionsNeedingSegmentationRequest struct {
	IdleCutoffMs int64 `json:"idle_cutoff_ms,omitempty"`
	IdleMs       int64 `json:"idle_ms,omitempty"`
	MinEvents    int   `json:"min_events,omitempty"`
	Limit        int   `json:"limit,omitempty"`
}

// SessionsNeedingSegmentationResponse is the body for
// /v1/sessions/needing-segmentation. Just the session ids — the
// caller pulls events for each via /v1/sessions/{id}/events.
type SessionsNeedingSegmentationResponse struct {
	SessionIDs []string `json:"session_ids"`
}

// SessionCompletion is the wire shape for one shell-completion
// row returned by GET /v1/sessions/completions. Mirrors
// store.SessionCompletion 1:1.
type SessionCompletion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// SessionCompletionsResponse is the body for /v1/sessions/completions.
type SessionCompletionsResponse struct {
	Sessions []SessionCompletion `json:"sessions"`
}

// InductionCandidate is the wire shape for one row of
// /v1/induction/candidates — the sessions ripe for online
// induction (idle long enough, ≥minEvents, no induction row yet).
type InductionCandidate struct {
	ID          string  `json:"id"`
	StartedAtMs *int64  `json:"started_at_ms,omitempty"`
	EndedAtMs   *int64  `json:"ended_at_ms,omitempty"`
	Cwd         *string `json:"cwd,omitempty"`
	EventCount  int     `json:"event_count"`
}

// InductionCandidatesRequest is the query-shape for
// GET /v1/induction/candidates.
type InductionCandidatesRequest struct {
	NowMs           int64 `json:"now_ms,omitempty"`
	IdleThresholdMs int64 `json:"idle_threshold_ms,omitempty"`
	MinEvents       int   `json:"min_events,omitempty"`
	Limit           int   `json:"limit,omitempty"`
}

// InductionCandidatesResponse is the body for /v1/induction/candidates.
type InductionCandidatesResponse struct {
	Candidates []InductionCandidate `json:"candidates"`
}

// FailureShape is the wire shape for one row of
// /v1/proposals/failure-shapes — the contrastive corpus the
// propose prompt sees alongside the positive digest list.
type FailureShape struct {
	SessionID         string  `json:"session_id"`
	EndedAtMs         *int64  `json:"ended_at_ms,omitempty"`
	Cwd               *string `json:"cwd,omitempty"`
	Title             string  `json:"title"`
	ToolFailureCount  int     `json:"tool_failure_count"`
	GitUndoCount      int     `json:"git_undo_count"`
	PromptRepeatCount int     `json:"prompt_repeat_count"`
	LastEventKind     *string `json:"last_event_kind,omitempty"`
}

// FailureShapesResponse is the body for /v1/proposals/failure-shapes.
type FailureShapesResponse struct {
	Shapes []FailureShape `json:"shapes"`
}

// SkillFailureContext is the wire shape for one row of
// /v1/skills/failures — a (load, failure) pair the
// `aichronicles skills evolve` prompt feeds the LLM as concrete
// evidence for the revision.
type SkillFailureContext struct {
	SessionID  string `json:"session_id"`
	LoadTsMs   int64  `json:"load_ts_ms"`
	FailTsMs   int64  `json:"fail_ts_ms"`
	FailBody   string `json:"fail_body"`
	NearbyText string `json:"nearby_text"`
}

// SkillFailuresRequest is the query-shape for /v1/skills/failures.
type SkillFailuresRequest struct {
	Skill    string `json:"skill"`
	SinceMs  int64  `json:"since_ms,omitempty"`
	WindowMs int64  `json:"window_ms,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// SkillFailuresResponse is the body for /v1/skills/failures.
type SkillFailuresResponse struct {
	Failures []SkillFailureContext `json:"failures"`
}

// SkillCandidateEffectiveness is the wire shape for one row of
// /v1/skill-candidates/effectiveness — post-add usage of a
// candidate the user accepted, joined to skill_load extractions
// and tool_failure correlation.
type SkillCandidateEffectiveness struct {
	CandidateID      int64  `json:"candidate_id"`
	LLMOutputID      int64  `json:"llm_output_id"`
	SkillName        string `json:"skill_name"`
	ProposedAtMs     int64  `json:"proposed_at_ms"`
	AddedAtMs        int64  `json:"added_at_ms"`
	AddPath          string `json:"add_path"`
	LoadsAfterAdd    int    `json:"loads_after_add"`
	FailedLoadsAfter int    `json:"failed_loads_after"`
	LastLoadedMs     *int64 `json:"last_loaded_ms,omitempty"`
}

// SkillCandidateEffectivenessRequest is the query-shape for
// /v1/skill-candidates/effectiveness.
type SkillCandidateEffectivenessRequest struct {
	SinceMs  int64 `json:"since_ms,omitempty"`
	WindowMs int64 `json:"window_ms,omitempty"`
	Limit    int   `json:"limit,omitempty"`
}

// SkillCandidateEffectivenessResponse is the body for
// /v1/skill-candidates/effectiveness.
type SkillCandidateEffectivenessResponse struct {
	Rows []SkillCandidateEffectiveness `json:"rows"`
}

// PendingSkillCandidatesResponse is the body for
// /v1/skill-candidates/pending. Reuses the SkillCandidate wire
// shape so callers walk one row type for both lifecycle reads.
type PendingSkillCandidatesResponse struct {
	Candidates []SkillCandidate `json:"candidates"`
}

// AddedSkillCandidateResponse is the body for
// /v1/skill-candidates/added?name=. Returns the most-recent row
// whose decision='add' for the named skill, or 404 when none
// exists (the skill is hand-authored).
type AddedSkillCandidateResponse struct {
	Candidate SkillCandidate `json:"candidate"`
}

// SegmentSessionRequest is the body for POST /v1/sessions/{id}/segment.
// Optional idle_gap_ms overrides the default segmentation gap; zero
// uses the store's canonical value.
type SegmentSessionRequest struct {
	IdleGapMs int64 `json:"idle_gap_ms,omitempty"`
}

// SegmentSessionResponse reports how many episodes the segmenter
// produced on this run.
type SegmentSessionResponse struct {
	Episodes int `json:"episodes"`
}

// UpdateSkillCandidateRequest is the body for
// PATCH-style POST /v1/skill-candidates/{id}/update. Carries the
// add-body-hash + kind refresh used by the merge path: once a
// merge writes the new SKILL.md, the surviving candidate row's
// stored hash and kind label have to converge to the post-merge
// reality.
type UpdateSkillCandidateRequest struct {
	AddPath    string `json:"add_path,omitempty"`
	BodySHA256 string `json:"body_sha256,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// UpdateSkillCandidateResponse is empty on success.
type UpdateSkillCandidateResponse struct{}

// VacuumResponse is the body for POST /v1/admin/vacuum. Empty on
// success; the operation is intentionally synchronous because
// VACUUM holds an exclusive lock and the caller needs to know it
// completed before issuing follow-up work.
type VacuumResponse struct{}

// DBPageInfoResponse is the body for GET /v1/admin/db-info. Mirrors
// store.PageInfo with the on-disk bytes computed server-side so
// the CLI's before/after vacuum report doesn't have to multiply.
type DBPageInfoResponse struct {
	PageCount int64 `json:"page_count"`
	PageSize  int64 `json:"page_size"`
	Bytes     int64 `json:"bytes"`
}

// IngestStatsResponse is the body for GET /v1/admin/stats. A
// snapshot of the ingest_pending queue: depth, age of the oldest
// row, and the worst row's attempt count. Lets an operator answer
// "is the worker keeping up?" with a single curl rather than
// journal-grepping for "ingest worker: row failed".
type IngestStatsResponse struct {
	// Pending is the current row count in ingest_pending.
	Pending int `json:"pending"`
	// Capacity is the configured backlog cap; once Pending hits
	// this the daemon returns 503 to new ingest POSTs.
	Capacity int `json:"capacity"`
	// OldestAgeMs is milliseconds since the oldest pending row's
	// received_at_ms. Zero when the queue is empty.
	OldestAgeMs int64 `json:"oldest_age_ms"`
	// MaxAttempts is the largest attempt_count across all pending
	// rows. Non-zero means at least one row has failed before.
	MaxAttempts int `json:"max_attempts"`
}
