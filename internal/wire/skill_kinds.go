package wire

// SkillKind labels what shape a candidate skill encodes — a
// success pattern ("when X fires, do Y") or a failure pitfall
// ("when X is about to fail, AVOID Y"). Lives in wire/ because
// it's protocol-level vocabulary: every JSON body carrying a kind
// string and every client filtering on one needs the same enum.
//
// EvoSkill (2603.02766) and EvoSC (2602.01966) argue that
// contrastive induction needs both forms; conflating them in one
// bank loses the negative-evidence half of the corpus. Stored on
// the skill_candidates row so the merge gate, retire signals, and
// SKILL.md frontmatter can branch on the kind without re-deriving
// it from the body text. Mirrors the LLMOutputKind and
// SessionLinkKind lifts (88025dc / 582d404). store.SkillKind is a
// type alias of this so existing call sites keep working unchanged.
type SkillKind string

const (
	// SkillKindPattern is the canonical success-driven form: the
	// LLM saw the same successful procedure across two or more
	// sessions and emitted a skill that codifies it.
	SkillKindPattern SkillKind = "pattern"

	// SkillKindPitfall is the failure-driven form: the LLM saw
	// the same recurring failure mode across two or more
	// failure_likely sessions and emitted a skill that names what
	// to avoid (or how to recover early). Loaded by Claude Code
	// the same way as a pattern skill; Claude reads its body and
	// follows the avoid-this guidance.
	SkillKindPitfall SkillKind = "pitfall"
)
