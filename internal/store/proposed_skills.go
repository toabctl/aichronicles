package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// ProposedSkillRow is one entry from the proposed_skills table —
// the lifecycle index for skills the LLM emitted via propose /
// induction. AppliedAtMs and AppliedPath are NULL until the user
// runs `propose apply`; SupersededByID is NULL until a newer
// proposal claims the same name.
type ProposedSkillRow struct {
	ID             int64
	LLMOutputID    int64
	SkillName      string
	ProposedAtMs   int64
	AppliedAtMs    sql.NullInt64
	AppliedPath    sql.NullString
	SupersededByID sql.NullInt64
}

// RecordProposedSkill inserts the (llm_output_id, skill_name) pair
// into proposed_skills if it isn't already present. INSERT OR IGNORE
// semantics: the natural-key UNIQUE constraint guarantees one row
// per pair, and re-running propose with a cache hit is a no-op.
//
// proposedAtMs is supplied by the caller (typically
// llm_outputs.created_at_ms) so the lifecycle timestamp is anchored
// to when the LLM emitted the proposal, not when this helper happens
// to run. Defensive zero-check: the schema requires proposed_at_ms
// NOT NULL; passing 0 would persist a meaningless epoch value.
func RecordProposedSkill(ctx context.Context, db *sql.DB, llmOutputID int64, skillName string, proposedAtMs int64) error {
	if llmOutputID <= 0 {
		return errors.New("RecordProposedSkill: llm_output_id is required")
	}
	if skillName == "" {
		return errors.New("RecordProposedSkill: skill_name is required")
	}
	if proposedAtMs <= 0 {
		return errors.New("RecordProposedSkill: proposed_at_ms is required")
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO proposed_skills(llm_output_id, skill_name, proposed_at_ms)
		 VALUES (?, ?, ?)
		 ON CONFLICT(llm_output_id, skill_name) DO NOTHING`,
		llmOutputID, skillName, proposedAtMs,
	)
	if err != nil {
		return fmt.Errorf("insert proposed_skills: %w", err)
	}
	return nil
}

// MarkProposedSkillApplied finds the (llm_output_id, skill_name)
// row and sets applied_at_ms + applied_path. The pair MUST already
// exist (from RecordProposedSkill at propose time); a missing row
// returns ErrProposedSkillNotFound so the caller can detect the
// "tried to apply something we never proposed" case rather than
// silently no-op-ing. The error is wrapped so callers can use
// errors.Is.
func MarkProposedSkillApplied(ctx context.Context, db *sql.DB, llmOutputID int64, skillName, appliedPath string, appliedAtMs int64) error {
	if appliedAtMs <= 0 {
		return errors.New("MarkProposedSkillApplied: applied_at_ms is required")
	}
	res, err := db.ExecContext(ctx,
		`UPDATE proposed_skills
		    SET applied_at_ms = ?, applied_path = ?
		  WHERE llm_output_id = ? AND skill_name = ?`,
		appliedAtMs, appliedPath, llmOutputID, skillName,
	)
	if err != nil {
		return fmt.Errorf("update applied: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: llm_output_id=%d skill=%q", ErrProposedSkillNotFound, llmOutputID, skillName)
	}
	return nil
}

// ErrProposedSkillNotFound is returned when MarkProposedSkillApplied
// addresses a (llm_output_id, skill_name) pair that has no row.
// Distinct from a database error — surfaces a missing
// RecordProposedSkill call upstream.
var ErrProposedSkillNotFound = errors.New("proposed_skills row not found")

// LoadProposedSkillsByName returns every proposed_skills row for
// skill_name across history, newest-first. Used by the CLI / web /
// MCP "is this skill on disk one that aichronicles proposed?" query
// — provenance for hand-authored vs. propose-applied skills.
//
// limit ≤0 falls back to 20.
func LoadProposedSkillsByName(ctx context.Context, db *sql.DB, skillName string, limit int) ([]ProposedSkillRow, error) {
	if skillName == "" {
		return nil, errors.New("LoadProposedSkillsByName: skill_name is required")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, llm_output_id, skill_name, proposed_at_ms,
		        applied_at_ms, applied_path, superseded_by_id
		   FROM proposed_skills
		  WHERE skill_name = ?
		  ORDER BY proposed_at_ms DESC
		  LIMIT ?`,
		skillName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query proposed_skills: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProposedSkillRow
	for rows.Next() {
		var r ProposedSkillRow
		if err := rows.Scan(&r.ID, &r.LLMOutputID, &r.SkillName, &r.ProposedAtMs,
			&r.AppliedAtMs, &r.AppliedPath, &r.SupersededByID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProposalEffectiveness summarises the post-apply usage of one
// proposed-and-applied skill. Computed by joining proposed_skills
// (where applied_at_ms IS NOT NULL) to extractions of
// kind=skill_load with value=skill_name. Failure correlation
// reuses the staleness window pattern (default 10 min — same as
// LoadSkillImpact) so this view stays consistent with the
// skills-staleness one.
//
// Distinct from LoadSkillImpact: that aggregates over EVERY
// skill_load extraction (regardless of whether the skill was
// proposed by aichronicles or hand-authored). This view restricts
// to skills aichronicles proposed AND the user applied — the
// closed-loop signal: "did our proposals actually help?"
type ProposalEffectiveness struct {
	ProposedSkillID  int64
	LLMOutputID      int64
	SkillName        string
	ProposedAtMs     int64
	AppliedAtMs      int64
	AppliedPath      string
	LoadsAfterApply  int
	FailedLoadsAfter int
	LastLoadedMs     sql.NullInt64
}

// LoadProposalEffectiveness returns one row per applied
// proposed_skills entry whose applied_at_ms is at or after sinceMs,
// joined to skill_load extractions and tool_failure correlation in
// the canonical 10-minute window. Most-recently-applied first.
//
// limit ≤0 falls back to 50. windowMs ≤0 uses defaultStalenessWindow.
func LoadProposalEffectiveness(ctx context.Context, db *sql.DB, sinceMs int64, windowMs int64, limit int) ([]ProposalEffectiveness, error) {
	if windowMs <= 0 {
		windowMs = defaultStalenessWindow
	}
	if limit <= 0 {
		limit = 50
	}
	const q = `
SELECT ps.id, ps.llm_output_id, ps.skill_name, ps.proposed_at_ms,
       ps.applied_at_ms, COALESCE(ps.applied_path, ''),
       (SELECT COUNT(*) FROM extractions x
          JOIN events e ON e.event_id = x.event_id
         WHERE x.kind = ? AND x.value = ps.skill_name
           AND e.ts_source_ms >= ps.applied_at_ms
       ) AS loads_after,
       (SELECT COUNT(*) FROM extractions x
          JOIN events e ON e.event_id = x.event_id
         WHERE x.kind = ? AND x.value = ps.skill_name
           AND e.ts_source_ms >= ps.applied_at_ms
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
         WHERE x.kind = ? AND x.value = ps.skill_name
           AND e.ts_source_ms >= ps.applied_at_ms
       ) AS last_loaded
  FROM proposed_skills ps
 WHERE ps.applied_at_ms IS NOT NULL
   AND ps.applied_at_ms >= ?
 ORDER BY ps.applied_at_ms DESC
 LIMIT ?`
	rows, err := db.QueryContext(ctx, q,
		extract.KindSkillLoad,
		extract.KindSkillLoad, ingest.KindToolFailure, windowMs,
		extract.KindSkillLoad,
		sinceMs, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query proposal effectiveness: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProposalEffectiveness
	for rows.Next() {
		var e ProposalEffectiveness
		if err := rows.Scan(&e.ProposedSkillID, &e.LLMOutputID, &e.SkillName, &e.ProposedAtMs,
			&e.AppliedAtMs, &e.AppliedPath,
			&e.LoadsAfterApply, &e.FailedLoadsAfter, &e.LastLoadedMs); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountUnappliedProposals returns the number of proposed_skills rows
// whose applied_at_ms IS NULL, optionally filtered by sinceMs (only
// proposals younger than the cutoff are counted). The
// abandonment-rate signal: how many of the LLM's suggestions did
// the user not act on?
//
// sinceMs ≤0 disables the filter (counts every unapplied row).
func CountUnappliedProposals(ctx context.Context, db *sql.DB, sinceMs int64) (int, error) {
	q := `SELECT COUNT(*) FROM proposed_skills WHERE applied_at_ms IS NULL`
	args := []any{}
	if sinceMs > 0 {
		q += ` AND proposed_at_ms >= ?`
		args = append(args, sinceMs)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unapplied: %w", err)
	}
	return n, nil
}
