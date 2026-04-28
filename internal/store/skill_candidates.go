package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
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
type SkillCandidate struct {
	ID           int64
	LLMOutputID  int64
	SkillName    string
	ProposedAtMs int64
	DecisionAtMs sql.NullInt64
	AddPath      sql.NullString
	Decision     MaintenanceAction
	MergedIntoID sql.NullInt64
}

// ErrSkillCandidateNotFound is returned by the Mark* helpers when
// the (llm_output_id, skill_name) pair has no matching row.
// Distinct from a database error — surfaces a missing
// RecordSkillCandidate call upstream.
var ErrSkillCandidateNotFound = errors.New("skill_candidates row not found")

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
func RecordSkillCandidate(ctx context.Context, db *sql.DB, llmOutputID int64, skillName string, proposedAtMs int64) error {
	if llmOutputID <= 0 {
		return errors.New("RecordSkillCandidate: llm_output_id is required")
	}
	if skillName == "" {
		return errors.New("RecordSkillCandidate: skill_name is required")
	}
	if proposedAtMs <= 0 {
		return errors.New("RecordSkillCandidate: proposed_at_ms is required")
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO skill_candidates(llm_output_id, skill_name, proposed_at_ms)
		 VALUES (?, ?, ?)
		 ON CONFLICT(llm_output_id, skill_name) DO NOTHING`,
		llmOutputID, skillName, proposedAtMs,
	)
	if err != nil {
		return fmt.Errorf("insert skill_candidates: %w", err)
	}
	return nil
}

// MarkSkillCandidateAdded sets decision='add' on the candidate row
// identified by (llm_output_id, skill_name) and records the on-disk
// SKILL.md path plus the timestamp of the maintenance decision.
// The pair MUST already exist (from a prior RecordSkillCandidate at
// extraction time); a missing row returns ErrSkillCandidateNotFound
// so the caller can detect the "marked added for something we never
// proposed" case rather than silently no-op-ing.
func MarkSkillCandidateAdded(ctx context.Context, db *sql.DB, llmOutputID int64, skillName, addPath string, decisionAtMs int64) error {
	if decisionAtMs <= 0 {
		return errors.New("MarkSkillCandidateAdded: decision_at_ms is required")
	}
	res, err := db.ExecContext(ctx,
		`UPDATE skill_candidates
		    SET decision       = ?,
		        decision_at_ms = ?,
		        add_path       = ?
		  WHERE llm_output_id = ? AND skill_name = ?`,
		string(MaintenanceAdd), decisionAtMs, addPath, llmOutputID, skillName,
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
		`SELECT id, llm_output_id, skill_name, proposed_at_ms,
		        decision_at_ms, add_path,
		        COALESCE(decision, ''), merged_into_id
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
		var r SkillCandidate
		var decision string
		if err := rows.Scan(&r.ID, &r.LLMOutputID, &r.SkillName, &r.ProposedAtMs,
			&r.DecisionAtMs, &r.AddPath, &decision, &r.MergedIntoID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Decision = MaintenanceAction(decision)
		out = append(out, r)
	}
	return out, rows.Err()
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
	q := `SELECT id, llm_output_id, skill_name, proposed_at_ms,
	             decision_at_ms, add_path,
	             COALESCE(decision, ''), merged_into_id
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
		var r SkillCandidate
		var decision string
		if err := rows.Scan(&r.ID, &r.LLMOutputID, &r.SkillName, &r.ProposedAtMs,
			&r.DecisionAtMs, &r.AddPath, &decision, &r.MergedIntoID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Decision = MaintenanceAction(decision)
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
