package wire

// SkillCandidateExample is one (input, output) demonstration in
// the AutoSkill ξ set — a representative user query paired with a
// short summary of what the skill does for it. Mirrors
// store.SkillExample.
type SkillCandidateExample struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// SkillCandidateMetadata carries the AutoSkill 7-tuple metadata
// (τ triggers, γ tags, ξ examples, v version, kind) shipped on a
// record-skill-candidate request.
type SkillCandidateMetadata struct {
	Triggers []string                `json:"triggers,omitempty"`
	Tags     []string                `json:"tags,omitempty"`
	Examples []SkillCandidateExample `json:"examples,omitempty"`
	Version  string                  `json:"version,omitempty"`
	// Kind labels what the candidate encodes — "pattern" (success-
	// driven, "do X") or "pitfall" (failure-driven, "avoid X").
	// Empty defaults server-side to "pattern".
	Kind string `json:"kind,omitempty"`
}

// RecordSkillCandidateRequest is the body for POST /v1/skill-candidates.
// Idempotent on (llm_output_id, skill_name); a re-record with new
// metadata folds metadata into the existing row via UPSERT.
type RecordSkillCandidateRequest struct {
	LLMOutputID  int64                  `json:"llm_output_id"`
	SkillName    string                 `json:"skill_name"`
	ProposedAtMs int64                  `json:"proposed_at_ms"`
	Metadata     SkillCandidateMetadata `json:"metadata,omitempty"`
}

// RecordSkillCandidateResponse echoes the canonical row id (when
// the candidate was newly inserted) or zero on idempotent re-record.
type RecordSkillCandidateResponse struct {
	Inserted bool `json:"inserted"`
}

// SkillCandidateDecisionRequest is the body for POST
// /v1/skill-candidates/decision. Decision is one of
// "add" | "merge" | "discard"; the optional fields are decision-
// specific:
//
//   - add:     AddPath required, BodySHA256 optional (SSGM provenance)
//   - merge:   AddPath required (the on-disk SKILL.md the merge
//     landed in), MergedIntoID optional (zero = merged into
//     a hand-authored skill with no candidate row)
//   - discard: no extra fields
type SkillCandidateDecisionRequest struct {
	LLMOutputID  int64  `json:"llm_output_id"`
	SkillName    string `json:"skill_name"`
	Decision     string `json:"decision"`
	DecisionAtMs int64  `json:"decision_at_ms"`
	AddPath      string `json:"add_path,omitempty"`
	BodySHA256   string `json:"body_sha256,omitempty"`
	MergedIntoID int64  `json:"merged_into_id,omitempty"`
}

// SkillCandidateDecisionResponse is the body for the decision
// endpoint; carries no payload on success.
type SkillCandidateDecisionResponse struct{}

// SkillCandidate is the wire shape for one /v1/skill-candidates
// list row. Mirrors store.SkillCandidate one-to-one with nullable
// columns projected to *T pointers.
type SkillCandidate struct {
	ID            int64                   `json:"id"`
	LLMOutputID   int64                   `json:"llm_output_id"`
	SkillName     string                  `json:"skill_name"`
	ProposedAtMs  int64                   `json:"proposed_at_ms"`
	DecisionAtMs  *int64                  `json:"decision_at_ms,omitempty"`
	AddPath       *string                 `json:"add_path,omitempty"`
	Decision      string                  `json:"decision"`
	MergedIntoID  *int64                  `json:"merged_into_id,omitempty"`
	Triggers      []string                `json:"triggers,omitempty"`
	Tags          []string                `json:"tags,omitempty"`
	Examples      []SkillCandidateExample `json:"examples,omitempty"`
	Version       string                  `json:"version,omitempty"`
	AddBodySHA256 *string                 `json:"add_body_sha256,omitempty"`
	Kind          string                  `json:"kind,omitempty"`
}

// SkillCandidatesResponse is the body for GET /v1/skill-candidates.
type SkillCandidatesResponse struct {
	Candidates []SkillCandidate `json:"candidates"`
}
