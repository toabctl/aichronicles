package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// InitialSkillVersion is the version stamp every newly-recorded
// candidate gets if no version is supplied. Follows AutoSkill's
// example examples (v0.1.0, v0.1.34) and standard semver-ish form;
// the merge path bumps the patch number on the existing skill so
// "v0.1.0 → v0.1.1 → v0.1.2" is the natural progression of a skill
// that gets refined over time.
const InitialSkillVersion = "v0.1.0"

// BumpPatch returns the supplied version with its patch component
// incremented by one. Accepts "vMAJOR.MINOR.PATCH" or
// "MAJOR.MINOR.PATCH" (the leading 'v' is preserved on output if
// it was present on input). Any malformed input — missing
// components, non-integer parts, empty string — returns
// InitialSkillVersion as a safe fallback so a corrupted
// frontmatter can't strand the merge path.
//
// Examples:
//
//	BumpPatch("v0.1.0")  → "v0.1.1"
//	BumpPatch("v0.1.42") → "v0.1.43"
//	BumpPatch("0.1.0")   → "0.1.1"
//	BumpPatch("v1.2.3")  → "v1.2.4"
//	BumpPatch("")        → "v0.1.0"
//	BumpPatch("garbage") → "v0.1.0"
func BumpPatch(version string) string {
	v := version
	hasV := false
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		hasV = true
		v = v[1:]
	}
	parts := splitVersion(v)
	if len(parts) != 3 {
		return InitialSkillVersion
	}
	major, ok1 := parseUint(parts[0])
	minor, ok2 := parseUint(parts[1])
	patch, ok3 := parseUint(parts[2])
	if !ok1 || !ok2 || !ok3 {
		return InitialSkillVersion
	}
	out := fmt.Sprintf("%d.%d.%d", major, minor, patch+1)
	if hasV {
		out = "v" + out
	}
	return out
}

// splitVersion splits "MAJOR.MINOR.PATCH" into three pieces. Stays
// in this file (rather than reaching for strings.Split) so the
// dependency footprint of the version helpers is just stdlib fmt.
func splitVersion(s string) []string {
	out := make([]string, 0, 3)
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[last:i])
			last = i + 1
		}
	}
	out = append(out, s[last:])
	return out
}

// parseUint accepts a non-empty digit-only string and returns its
// integer value. Empty / non-digit input returns (0, false). Used
// only by BumpPatch — accept-only-ASCII-digits is the right shape
// for semver-ish version components.
func parseUint(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// SkillExample is one (input, output) demonstration in the
// AutoSkill ξ set — a representative user query paired with a short
// summary of what the skill does for it. Stored as a JSON array of
// these objects in the skill_candidates.examples column.
type SkillExample struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// SkillKind labels what shape a candidate skill encodes — a
// success pattern ("when X fires, do Y") or a failure pitfall
// ("when X is about to fail, AVOID Y"). EvoSkill (2603.02766) and
// EvoSC (2602.01966) argue that contrastive induction needs both
// forms; conflating them in one bank loses the negative-evidence
// half of the corpus. Stored on the skill_candidates row so the
// merge gate, retire signals, and SKILL.md frontmatter can branch
// on the kind without re-deriving it from the body text.
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

// MaintenanceAction names the AutoSkill (Yang et al., 2026 —
// arXiv:2603.01145) maintenance decisions. After a candidate skill
// is extracted from session experience, the user picks exactly one
// of these actions: add a fresh on-disk SKILL.md, merge into an
// existing skill, or discard the candidate. The empty string is the
// pending state — extracted but not yet decided.
type MaintenanceAction string

const (
	// MaintenancePending is the default state of a freshly recorded
	// candidate. No on-disk artefact has been created and no
	// decision has been written.
	MaintenancePending MaintenanceAction = ""

	// MaintenanceAdd is the AutoSkill action for "this candidate is
	// a new skill" — a SKILL.md is materialised on disk at AddPath.
	MaintenanceAdd MaintenanceAction = "add"

	// MaintenanceMerge is the AutoSkill action for "this candidate
	// refines an existing skill" — the LLM-merged result is written
	// to the existing skill's path; MergedIntoID points at the
	// surviving candidate row.
	MaintenanceMerge MaintenanceAction = "merge"

	// MaintenanceDiscard is the AutoSkill action for "this candidate
	// is not worth keeping" — recorded so future propose runs can
	// see what the user rejected and bias away from re-suggesting.
	MaintenanceDiscard MaintenanceAction = "discard"
)

// SkillCandidate is one row of the skill_candidates table — the
// lifecycle-tracking record for a skill the LLM proposed via the
// induction / propose paths. AutoSkill names this object the
// "skill candidate" in the experience-ingestion → extraction →
// maintenance → reuse loop; aichronicles uses the same vocabulary
// throughout to avoid translating between an in-house term and the
// research literature.
//
// Decision captures the maintenance action (∈ {add, merge,
// discard}) the user took. MaintenancePending (empty) means the
// candidate is extracted but not yet acted on — the abandonment
// signal a future propose run uses to avoid re-suggesting the same
// idea. AddPath is set only for Decision==MaintenanceAdd;
// MergedIntoID is set only for Decision==MaintenanceMerge.
//
// Triggers, Tags, Examples, and Version are the AutoSkill 7-tuple
// metadata (τ, γ, ξ, v) — populated when the candidate is recorded
// via RecordSkillCandidateWithMetadata. Empty for legacy rows
// recorded before metadata persistence shipped.
type SkillCandidate struct {
	ID           int64
	LLMOutputID  int64
	SkillName    string
	ProposedAtMs int64
	DecisionAtMs sql.NullInt64
	AddPath      sql.NullString
	Decision     MaintenanceAction
	MergedIntoID sql.NullInt64
	Triggers     []string
	Tags         []string
	Examples     []SkillExample
	Version      string
	// AddBodySHA256 is the SHA-256 of the rendered SKILL.md body
	// captured at write time, when Decision == MaintenanceAdd. Per
	// SSGM (Lam et al., 2026 — arXiv:2603.11768) governance, an
	// integrity hash on the on-disk artefact lets a later sweep
	// distinguish "what aichronicles wrote" from "what was edited
	// after the fact." Empty / NULL when the candidate was never
	// added or was added before migration 023.
	AddBodySHA256 sql.NullString

	// Kind is the contrastive-induction label: pattern (success-
	// driven, "do X") or pitfall (failure-driven, "avoid X").
	// Defaults to SkillKindPattern for legacy rows via migration
	// 024's column default.
	Kind SkillKind
}

// ErrSkillCandidateNotFound is returned by the Mark* helpers when
// the (llm_output_id, skill_name) pair has no matching row.
// Distinct from a database error — surfaces a missing
// RecordSkillCandidate call upstream.
var ErrSkillCandidateNotFound = errors.New("skill_candidates row not found")

// SkillCandidateMetadata is the AutoSkill 7-tuple metadata
// (τ triggers, γ tags, ξ examples, v version) the LLM emits
// alongside the rest of the proposal. Persisted as JSON columns on
// the skill_candidates row so the merge path can read what the
// previous run captured without re-asking the LLM. All fields are
// optional; missing pieces just store as NULL.
type SkillCandidateMetadata struct {
	Triggers []string
	Tags     []string
	Examples []SkillExample
	// Version stamps the candidate row. Empty falls back to
	// InitialSkillVersion at write time.
	Version string
	// Kind labels the candidate as a success pattern or a failure
	// pitfall (contrastive induction). Empty defaults to
	// SkillKindPattern at write time — the existing aichronicles
	// emission shape.
	Kind SkillKind
}

// RecordSkillCandidate inserts the (llm_output_id, skill_name) pair
// into skill_candidates if it isn't already present (INSERT OR
// IGNORE on the natural-key UNIQUE). Re-running propose with a
// cache hit is a no-op.
//
// proposedAtMs is supplied by the caller (typically
// llm_outputs.created_at_ms) so the lifecycle timestamp is anchored
// to when the LLM emitted the proposal, not when this helper
// happens to run. Defensive zero-check: the schema requires
// proposed_at_ms NOT NULL; passing 0 would persist a meaningless
// epoch value.
//
// This thin form is kept for code paths that don't have AutoSkill
// metadata to hand (legacy paths, tests). New writers should prefer
// RecordSkillCandidateWithMetadata so triggers / tags / examples /
// version land on the row at extraction time, not after.
func RecordSkillCandidate(ctx context.Context, db *sql.DB, llmOutputID int64, skillName string, proposedAtMs int64) error {
	return RecordSkillCandidateWithMetadata(ctx, db, llmOutputID, skillName, proposedAtMs, SkillCandidateMetadata{})
}

// RecordSkillCandidateWithMetadata is the AutoSkill-aware writer:
// stores triggers / tags / examples / version on the row in the
// same INSERT that creates it. INSERT OR IGNORE on the natural-key
// UNIQUE keeps re-runs idempotent; metadata on a duplicate name
// (rare, only happens if RecordSkillCandidate landed first then
// metadata was filled in later) goes through DO UPDATE so the
// row converges to the metadata-rich form.
func RecordSkillCandidateWithMetadata(ctx context.Context, db *sql.DB, llmOutputID int64, skillName string, proposedAtMs int64, meta SkillCandidateMetadata) error {
	if llmOutputID <= 0 {
		return errors.New("RecordSkillCandidate: llm_output_id is required")
	}
	if skillName == "" {
		return errors.New("RecordSkillCandidate: skill_name is required")
	}
	if proposedAtMs <= 0 {
		return errors.New("RecordSkillCandidate: proposed_at_ms is required")
	}

	triggersJSON, err := marshalSkillStringList("triggers", meta.Triggers)
	if err != nil {
		return err
	}
	tagsJSON, err := marshalSkillStringList("tags", meta.Tags)
	if err != nil {
		return err
	}
	examplesJSON, err := marshalSkillExamples(meta.Examples)
	if err != nil {
		return err
	}
	version := meta.Version
	if version == "" {
		version = InitialSkillVersion
	}
	kind := meta.Kind
	if kind == "" {
		kind = SkillKindPattern
	}

	_, err = db.ExecContext(ctx,
		`INSERT INTO skill_candidates(
			llm_output_id, skill_name, proposed_at_ms,
			triggers, tags, examples, version, kind
		)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(llm_output_id, skill_name) DO UPDATE SET
		     triggers = COALESCE(excluded.triggers, skill_candidates.triggers),
		     tags     = COALESCE(excluded.tags,     skill_candidates.tags),
		     examples = COALESCE(excluded.examples, skill_candidates.examples),
		     version  = COALESCE(skill_candidates.version, excluded.version),
		     kind     = excluded.kind`,
		llmOutputID, skillName, proposedAtMs,
		nullableJSON(triggersJSON), nullableJSON(tagsJSON), nullableJSON(examplesJSON), version, string(kind),
	)
	if err != nil {
		return fmt.Errorf("insert skill_candidates: %w", err)
	}
	return nil
}

// marshalSkillStringList serialises a string slice to a JSON array,
// returning empty bytes when the slice is empty so the caller can
// store NULL rather than "[]". The label feeds the error message.
func marshalSkillStringList(label string, in []string) ([]byte, error) {
	if len(in) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", label, err)
	}
	return b, nil
}

// marshalSkillExamples serialises the AutoSkill ξ examples as a JSON
// array. Empty slice → nil bytes (column stays NULL).
func marshalSkillExamples(in []SkillExample) ([]byte, error) {
	if len(in) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal examples: %w", err)
	}
	return b, nil
}

// nullableJSON converts a possibly-empty JSON byte slice into the
// any value Go's database/sql driver maps to NULL when nil. Lets
// the INSERT bind NULL for absent metadata without writing two
// branches per call site.
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// scanSkillStringList parses a JSON-array column into a []string,
// tolerating NULL (returns nil) and empty strings. A malformed body
// returns an error so a corrupted row surfaces rather than silently
// giving an empty list.
func scanSkillStringList(s sql.NullString) ([]string, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil, fmt.Errorf("decode string list: %w", err)
	}
	return out, nil
}

// scanSkillExamples parses the JSON-array examples column into
// []SkillExample. NULL / empty → nil; malformed body → error.
func scanSkillExamples(s sql.NullString) ([]SkillExample, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var out []SkillExample
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil, fmt.Errorf("decode examples: %w", err)
	}
	return out, nil
}

// MarkSkillCandidateAdded sets decision='add' on the candidate row
// identified by (llm_output_id, skill_name) and records the on-disk
// SKILL.md path plus the timestamp of the maintenance decision.
// The pair MUST already exist (from a prior RecordSkillCandidate at
// extraction time); a missing row returns ErrSkillCandidateNotFound
// so the caller can detect the "marked added for something we never
// proposed" case rather than silently no-op-ing.
//
// Thin wrapper around MarkSkillCandidateAddedWithProvenance that
// passes an empty body hash. Suitable for callers (mainly tests)
// that don't have the rendered body to hand. Production callers
// should use the WithProvenance form so SSGM tamper-detection has
// something to compare against.
func MarkSkillCandidateAdded(ctx context.Context, db *sql.DB, llmOutputID int64, skillName, addPath string, decisionAtMs int64) error {
	return MarkSkillCandidateAddedWithProvenance(ctx, db, llmOutputID, skillName, addPath, decisionAtMs, "")
}

// MarkSkillCandidateAddedWithProvenance is the SSGM-aware variant:
// also stores the SHA-256 of the rendered SKILL.md body so a later
// drift check can compare what is on disk against what aichronicles
// wrote. Empty bodySHA256 leaves the column NULL — same observable
// shape as a pre-migration-023 row.
func MarkSkillCandidateAddedWithProvenance(ctx context.Context, db *sql.DB, llmOutputID int64, skillName, addPath string, decisionAtMs int64, bodySHA256 string) error {
	if decisionAtMs <= 0 {
		return errors.New("MarkSkillCandidateAdded: decision_at_ms is required")
	}
	var hashArg any
	if bodySHA256 != "" {
		hashArg = bodySHA256
	} // else nil → SQL NULL
	res, err := db.ExecContext(ctx,
		`UPDATE skill_candidates
		    SET decision        = ?,
		        decision_at_ms  = ?,
		        add_path        = ?,
		        add_body_sha256 = ?
		  WHERE llm_output_id = ? AND skill_name = ?`,
		string(MaintenanceAdd), decisionAtMs, addPath, hashArg, llmOutputID, skillName,
	)
	if err != nil {
		return fmt.Errorf("mark add: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: llm_output_id=%d skill=%q", ErrSkillCandidateNotFound, llmOutputID, skillName)
	}
	return nil
}

// MarkSkillCandidateMerged sets decision='merge' on the candidate
// row and records which existing candidate it was merged into. The
// merge target is itself a skill_candidates row whose Decision is
// (or eventually will be) MaintenanceAdd — the on-disk SKILL.md
// AutoSkill's "skill merger" prompt rewrites in place.
//
// mergedIntoID is required: a merge with no target is meaningless.
// addPath is also recorded so callers can reach the on-disk file
// directly (it duplicates the target row's add_path; storing it
// here keeps the lifecycle row self-contained for reporting).
func MarkSkillCandidateMerged(ctx context.Context, db *sql.DB, llmOutputID int64, skillName string, mergedIntoID int64, addPath string, decisionAtMs int64) error {
	if decisionAtMs <= 0 {
		return errors.New("MarkSkillCandidateMerged: decision_at_ms is required")
	}
	if mergedIntoID <= 0 {
		return errors.New("MarkSkillCandidateMerged: merged_into_id is required")
	}
	res, err := db.ExecContext(ctx,
		`UPDATE skill_candidates
		    SET decision       = ?,
		        decision_at_ms = ?,
		        merged_into_id = ?,
		        add_path       = ?
		  WHERE llm_output_id = ? AND skill_name = ?`,
		string(MaintenanceMerge), decisionAtMs, mergedIntoID, addPath, llmOutputID, skillName,
	)
	if err != nil {
		return fmt.Errorf("mark merge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: llm_output_id=%d skill=%q", ErrSkillCandidateNotFound, llmOutputID, skillName)
	}
	return nil
}

// MarkSkillCandidateDiscarded sets decision='discard' on the
// candidate row. No path or target — the user actively rejected the
// suggestion. The recorded decision biases future propose runs away
// from re-emitting the same kebab-name idea.
func MarkSkillCandidateDiscarded(ctx context.Context, db *sql.DB, llmOutputID int64, skillName string, decisionAtMs int64) error {
	if decisionAtMs <= 0 {
		return errors.New("MarkSkillCandidateDiscarded: decision_at_ms is required")
	}
	res, err := db.ExecContext(ctx,
		`UPDATE skill_candidates
		    SET decision       = ?,
		        decision_at_ms = ?
		  WHERE llm_output_id = ? AND skill_name = ?`,
		string(MaintenanceDiscard), decisionAtMs, llmOutputID, skillName,
	)
	if err != nil {
		return fmt.Errorf("mark discard: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: llm_output_id=%d skill=%q", ErrSkillCandidateNotFound, llmOutputID, skillName)
	}
	return nil
}

// scanSkillCandidate scans the canonical column list (id,
// llm_output_id, skill_name, proposed_at_ms, decision_at_ms,
// add_path, decision, merged_into_id, triggers, tags, examples,
// version) into a SkillCandidate. Centralised because three
// loaders all SELECT this same projection — drift between them
// would mean the AutoSkill metadata silently disappears from one
// of the call paths.
func scanSkillCandidate(rows *sql.Rows) (SkillCandidate, error) {
	var r SkillCandidate
	var (
		decision    string
		triggersStr sql.NullString
		tagsStr     sql.NullString
		examplesStr sql.NullString
		versionStr  sql.NullString
		kindStr     string
	)
	if err := rows.Scan(&r.ID, &r.LLMOutputID, &r.SkillName, &r.ProposedAtMs,
		&r.DecisionAtMs, &r.AddPath, &decision, &r.MergedIntoID,
		&triggersStr, &tagsStr, &examplesStr, &versionStr,
		&r.AddBodySHA256, &kindStr); err != nil {
		return SkillCandidate{}, fmt.Errorf("scan: %w", err)
	}
	r.Decision = MaintenanceAction(decision)
	if kindStr == "" {
		kindStr = string(SkillKindPattern)
	}
	r.Kind = SkillKind(kindStr)

	triggers, err := scanSkillStringList(triggersStr)
	if err != nil {
		return SkillCandidate{}, err
	}
	r.Triggers = triggers
	tags, err := scanSkillStringList(tagsStr)
	if err != nil {
		return SkillCandidate{}, err
	}
	r.Tags = tags
	examples, err := scanSkillExamples(examplesStr)
	if err != nil {
		return SkillCandidate{}, err
	}
	r.Examples = examples
	if versionStr.Valid {
		r.Version = versionStr.String
	}
	return r, nil
}

// candidateColumns is the canonical SELECT list for SkillCandidate
// scans. Keep in lockstep with scanSkillCandidate's column order.
const candidateColumns = `id, llm_output_id, skill_name, proposed_at_ms,
		        decision_at_ms, add_path,
		        COALESCE(decision, ''), merged_into_id,
		        triggers, tags, examples, version,
		        add_body_sha256, kind`

// LoadSkillCandidatesByName returns every skill_candidates row for
// the given skill name across history, newest-first. Used by the
// CLI / web / MCP "is this skill on disk one that aichronicles
// proposed?" query — provenance for hand-authored vs. propose-added
// skills.
//
// limit ≤0 falls back to 20.
func LoadSkillCandidatesByName(ctx context.Context, db *sql.DB, skillName string, limit int) ([]SkillCandidate, error) {
	if skillName == "" {
		return nil, errors.New("LoadSkillCandidatesByName: skill_name is required")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+candidateColumns+`
		   FROM skill_candidates
		  WHERE skill_name = ?
		  ORDER BY proposed_at_ms DESC
		  LIMIT ?`,
		skillName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query skill_candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SkillCandidate
	for rows.Next() {
		r, err := scanSkillCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadAddedSkillCandidate returns the most recent skill_candidates
// row whose Decision==MaintenanceAdd for the given skill name. Used
// by the merge path to find the surviving candidate that owns the
// on-disk SKILL.md (the merge target). Returns (nil, nil) when no
// added candidate exists for the name — the caller then knows the
// skill is hand-authored and there is no candidate to merge into.
func LoadAddedSkillCandidate(ctx context.Context, db *sql.DB, skillName string) (*SkillCandidate, error) {
	if skillName == "" {
		return nil, errors.New("LoadAddedSkillCandidate: skill_name is required")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+candidateColumns+`
		   FROM skill_candidates
		  WHERE skill_name = ?
		    AND decision = ?
		  ORDER BY decision_at_ms DESC
		  LIMIT 1`,
		skillName, string(MaintenanceAdd),
	)
	if err != nil {
		return nil, fmt.Errorf("query added candidate: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	r, err := scanSkillCandidate(rows)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SkillCandidateEffectiveness summarises the post-add usage of one
// candidate that the user accepted (Decision==MaintenanceAdd).
// Computed by joining skill_candidates to skill_load extractions
// and tool_failure correlation in the canonical 10-minute window.
//
// Distinct from LoadSkillImpact: that aggregates over EVERY
// skill_load extraction (regardless of whether the skill was
// proposed by aichronicles or hand-authored). This view restricts
// to skills aichronicles proposed AND the user added — the
// closed-loop signal: "did our suggestions actually help?"
type SkillCandidateEffectiveness struct {
	CandidateID      int64
	LLMOutputID      int64
	SkillName        string
	ProposedAtMs     int64
	AddedAtMs        int64
	AddPath          string
	LoadsAfterAdd    int
	FailedLoadsAfter int
	LastLoadedMs     sql.NullInt64
}

// LoadSkillCandidateEffectiveness returns one row per added
// candidate whose decision_at_ms is at or after sinceMs, joined to
// skill_load extractions and tool_failure correlation in the
// canonical 10-minute window. Most-recently-added first.
//
// limit ≤0 falls back to 50. windowMs ≤0 uses defaultStalenessWindow.
func LoadSkillCandidateEffectiveness(ctx context.Context, db *sql.DB, sinceMs int64, windowMs int64, limit int) ([]SkillCandidateEffectiveness, error) {
	if windowMs <= 0 {
		windowMs = defaultStalenessWindow
	}
	if limit <= 0 {
		limit = 50
	}
	const q = `
SELECT sc.id, sc.llm_output_id, sc.skill_name, sc.proposed_at_ms,
       sc.decision_at_ms, COALESCE(sc.add_path, ''),
       (SELECT COUNT(*) FROM extractions x
          JOIN events e ON e.event_id = x.event_id
         WHERE x.kind = ? AND x.value = sc.skill_name
           AND e.ts_source_ms >= sc.decision_at_ms
       ) AS loads_after,
       (SELECT COUNT(*) FROM extractions x
          JOIN events e ON e.event_id = x.event_id
         WHERE x.kind = ? AND x.value = sc.skill_name
           AND e.ts_source_ms >= sc.decision_at_ms
           AND EXISTS (
               SELECT 1 FROM events f
                WHERE f.session_id = e.session_id
                  AND f.kind       = ?
                  AND f.ts_source_ms >  e.ts_source_ms
                  AND f.ts_source_ms <= e.ts_source_ms + ?
           )
       ) AS failed_after,
       (SELECT MAX(e.ts_source_ms) FROM extractions x
          JOIN events e ON e.event_id = x.event_id
         WHERE x.kind = ? AND x.value = sc.skill_name
           AND e.ts_source_ms >= sc.decision_at_ms
       ) AS last_loaded
  FROM skill_candidates sc
 WHERE sc.decision = ?
   AND sc.decision_at_ms IS NOT NULL
   AND sc.decision_at_ms >= ?
 ORDER BY sc.decision_at_ms DESC
 LIMIT ?`
	rows, err := db.QueryContext(ctx, q,
		extract.KindSkillLoad,
		extract.KindSkillLoad, ingest.KindToolFailure, windowMs,
		extract.KindSkillLoad,
		string(MaintenanceAdd), sinceMs, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query skill candidate effectiveness: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SkillCandidateEffectiveness
	for rows.Next() {
		var e SkillCandidateEffectiveness
		if err := rows.Scan(&e.CandidateID, &e.LLMOutputID, &e.SkillName, &e.ProposedAtMs,
			&e.AddedAtMs, &e.AddPath,
			&e.LoadsAfterAdd, &e.FailedLoadsAfter, &e.LastLoadedMs); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LoadPendingSkillCandidates returns skill_candidates rows whose
// Decision is MaintenancePending (no maintenance action recorded
// yet), sorted newest-proposed first. Used by the propose prompt to
// surface "the LLM has suggested these and the user did not act on
// them" — the abandonment signal that biases the next proposal away
// from re-suggesting candidates the user has implicitly declined.
//
// Only one row is returned per skill_name (the newest proposal),
// even if multiple llm_outputs rows propose the same name. Older
// duplicates are filtered Go-side after the SQL ORDER BY.
//
// limit ≤0 falls back to 50. sinceMs ≤0 disables the time filter.
func LoadPendingSkillCandidates(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]SkillCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + candidateColumns + `
	        FROM skill_candidates
	       WHERE decision IS NULL`
	args := []any{}
	if sinceMs > 0 {
		q += ` AND proposed_at_ms >= ?`
		args = append(args, sinceMs)
	}
	q += ` ORDER BY proposed_at_ms DESC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load pending: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]struct{})
	var out []SkillCandidate
	for rows.Next() {
		r, err := scanSkillCandidate(rows)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[r.SkillName]; dup {
			continue
		}
		seen[r.SkillName] = struct{}{}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// CountPendingSkillCandidates returns the number of skill_candidates
// rows whose Decision is MaintenancePending, optionally filtered by
// sinceMs (only candidates proposed after the cutoff are counted).
// The abandonment-rate signal: how many of the LLM's suggestions
// did the user not act on?
//
// sinceMs ≤0 disables the filter (counts every pending row).
func CountPendingSkillCandidates(ctx context.Context, db *sql.DB, sinceMs int64) (int, error) {
	q := `SELECT COUNT(*) FROM skill_candidates WHERE decision IS NULL`
	args := []any{}
	if sinceMs > 0 {
		q += ` AND proposed_at_ms >= ?`
		args = append(args, sinceMs)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}
