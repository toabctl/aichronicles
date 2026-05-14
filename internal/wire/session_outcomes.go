package wire

// SessionOutcome is the wire shape for one session_outcomes row.
// Returned by GET /v1/sessions/{id}/outcome (with read-or-backfill
// semantics — a previously-uncomputed session lands a fresh row on
// the first read), and consumed by reflect / propose / induction
// prompts as a per-session prior. The label is a HEURISTIC over
// observable signals; downstream prompts treat it as a prior, not
// ground truth.
//
// Lives in wire/ because the same struct is what the API serves and
// what the prompts package consumes — no store-specific tags or
// columns. store.SessionOutcome is now a type alias of this so any
// existing internal/store/-importing call site keeps working
// unchanged.
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
	// Outcome is the verdict label. Typed via OutcomeLabel so
	// callers don't have to cast across the wire boundary; the
	// JSON shape is still a plain string.
	Outcome OutcomeLabel `json:"outcome"`
}

// OutcomeLabel is the coarse derived verdict for a session — a
// heuristic over observational signals, not ground truth. Downstream
// consumers read these as priors, not facts.
//
// Lives in wire/ because the verdict ships over the wire (GET
// /v1/sessions/{id}/outcome) and the propose / reflect prompts
// branch on it. Mirrors the LLMOutputKind / SessionLink / SkillKind
// lifts: store.OutcomeLabel is now a type alias of this so the
// existing store.Outcome* call sites keep working unchanged.
type OutcomeLabel string

const (
	// OutcomeSuccessLikely — clean activity, no failure markers.
	// Defined as: tool_use_count >= 1 AND tool_failure_count == 0
	// AND git_undo_count == 0 AND prompt_repeat_count == 0 AND
	// error_count == 0.
	OutcomeSuccessLikely OutcomeLabel = "success_likely"

	// OutcomeFailureLikely — strong failure signals: tool-failure
	// rate over a session-size-scaled floor, a git undo, two
	// consecutive prompt repeats, or "ended on tool_failure or
	// error" with at least one failure-class event.
	OutcomeFailureLikely OutcomeLabel = "failure_likely"

	// OutcomeMixed — real activity with weak failure signals. The
	// session got somewhere but had friction. Used when the row
	// fails the success bar but doesn't trip the failure bar.
	OutcomeMixed OutcomeLabel = "mixed"

	// OutcomeUnknown — too thin to label. tool_use_count == 0 and
	// user_prompt_count <= 1 — typically aborted preambles, never
	// got far enough to leave a trail.
	OutcomeUnknown OutcomeLabel = "unknown"
)
