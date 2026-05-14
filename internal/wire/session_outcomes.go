package wire

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
