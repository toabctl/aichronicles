package wire

// LLMOutputKind is the discriminator for the `kind` column of the
// llm_outputs cache. Lives in wire/ rather than store/ because it's
// protocol-level vocabulary — every JSON body carrying a kind string,
// every client filtering on one, needs the same set. The DB column is
// still free text, so a new kind doesn't need a migration; this enum
// is the application-level contract that keeps producers and consumers
// in sync. (store.LLMOutputKind is a type alias for this, kept so the
// 126+ existing call sites don't have to change in lockstep.)
type LLMOutputKind string

const (
	LLMKindSummary       LLMOutputKind = "summary"
	LLMKindReflect       LLMOutputKind = "reflect"
	LLMKindPropose       LLMOutputKind = "propose"
	LLMKindReflectWeekly LLMOutputKind = "reflect_weekly"
	// LLMKindProposeVerify is the cached output of the critic LLM
	// pass that `propose add` runs before writing a SKILL.md
	// (Voyager-style verification gate). One row per (proposal-id,
	// skill-name) pair so re-running apply on the same skill is
	// free.
	LLMKindProposeVerify LLMOutputKind = "propose_verify"
	// LLMKindSkillRevision is the cached output of `aichronicles
	// skills evolve` — a revision of an existing SKILL.md
	// grounded in the failure events the staleness detector
	// flagged. One row per (skill-name, current-skill-md-hash)
	// so re-running on the same SKILL contents is free; a hand-
	// edit to the SKILL.md invalidates the cache automatically.
	LLMKindSkillRevision LLMOutputKind = "skill_revision"
	// LLMKindInduction is the cached output of online induction
	// — single-session propose triggered the moment a session
	// idles out. One row per (session_id, prompt-hash) so
	// re-running on the same session contents hits the cache.
	// Distinguished from LLMKindPropose so the CLI listing can
	// segregate "skills surfaced from one session by the auto
	// trigger" from "skills surfaced from a multi-session window
	// by the user".
	LLMKindInduction LLMOutputKind = "induction"
	// LLMKindChallenge is the cached output of `propose
	// --challenge`: forward-looking next-problem suggestions
	// derived from the same digest list propose uses, plus open
	// threads from prior sessions. Voyager's automatic-curriculum
	// analog. Separate from LLMKindPropose so the CLI listing
	// distinguishes "skills surfaced from past patterns" from
	// "challenges I should tackle next".
	LLMKindChallenge LLMOutputKind = "challenge"
	// LLMKindFacts is the cached output of single-session SEMANTIC
	// fact induction. The LLM extracts typed (subject, predicate,
	// object) triples from the session — project-level facts like
	// "uses Go 1.26", "runs tests via go test ./..." — and the
	// caller persists them into the semantic_facts table for typed
	// retrieval. The llm_outputs row holds the raw LLM reply for
	// caching + auditability; the truth lives in semantic_facts.
	LLMKindFacts LLMOutputKind = "facts"
	// LLMKindSkillMerge is the cached output of `aichronicles
	// propose merge` — the AutoSkill (Yang et al., 2026) maintenance
	// action 'merge' that combines an existing SKILL.md with a
	// freshly-extracted candidate. One row per (output-id, skill-name)
	// pair so re-running merge on the same proposal is free. Distinct
	// from LLMKindSkillRevision: revision tightens an existing skill
	// against its observed failures; merge folds a new candidate into
	// an existing skill that's working but could be enriched.
	LLMKindSkillMerge LLMOutputKind = "skill_merge"
)

// LLMOutput is the wire shape for one llm_outputs cache row,
// returned by /v1/llm-outputs and /v1/summaries. Maps from
// store.LLMOutput at the handler boundary.
type LLMOutput struct {
	ID           int64   `json:"id"`
	SessionID    *string `json:"session_id,omitempty"`
	Kind         string  `json:"kind"`
	Model        string  `json:"model"`
	PromptHash   string  `json:"prompt_hash"`
	InputTokens  *int64  `json:"input_tokens,omitempty"`
	OutputTokens *int64  `json:"output_tokens,omitempty"`

	// CacheWriteTokens / CacheReadTokens are Anthropic's prompt-cache
	// counters, which the API reports separately from input_tokens.
	// Omitted (nil) for rows written before they were captured, which
	// is distinct from a real 0 on a call that used no cache.
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`

	Body        string `json:"body"`
	CreatedAtMs int64  `json:"created_at_ms"`
}

// SummariesBatchResponse is the body for GET /v1/summaries/batch.
// Keyed by session_id; sessions without a cached summary are simply
// absent from the map (no "null" sentinel). Used by the web's
// session-list page to enrich N rows with their latest summary in a
// single round-trip, avoiding N HTTP calls.
type SummariesBatchResponse struct {
	Summaries map[string]LLMOutput `json:"summaries"`
}

// LLMOutputsListResponse is the body for GET /v1/llm-outputs and
// GET /v1/sessions/{id}/llm-outputs. NextCursor (via PageResponse)
// pages the cross-session list; it stays empty for the per-session
// sub-resource, which returns a session's outputs in full.
type LLMOutputsListResponse struct {
	Outputs []LLMOutput `json:"outputs"`
	PageResponse
}

// LLMOutputLastCreatedAtResponse is the body for
// GET /v1/llm-outputs/last-created-at?kind=. LastCreatedAtMs is 0
// when no rows of that kind exist.
type LLMOutputLastCreatedAtResponse struct {
	LastCreatedAtMs int64 `json:"last_created_at_ms"`
}

// LLMOutputExistsResponse is the body for
// GET /v1/llm-outputs/exists?session_id=&kind=.
type LLMOutputExistsResponse struct {
	Exists bool `json:"exists"`
}
