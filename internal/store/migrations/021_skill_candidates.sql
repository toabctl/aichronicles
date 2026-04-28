-- Migration 021: adopt the AutoSkill (Yang et al., 2026 — arXiv:2603.01145)
-- vocabulary and lifecycle for the candidate-skill table.
--
-- AutoSkill formalises the field's standard skill-induction loop as
--
--   experience ingestion → skill extraction → skill maintenance → skill reuse
--
-- where each newly-extracted candidate is followed by exactly one
-- maintenance decision a_t ∈ {add, merge, discard}. Pre-migration
-- aichronicles tracked only an implicit two-state version of this
-- lifecycle (applied_at_ms set ↔ "added"; NULL ↔ "pending"), which
-- conflated "the user has not decided yet" with "the user actively
-- declined" and gave no place to record a merge target. This
-- migration aligns the schema with the standard vocabulary so the
-- merge-not-replace path can be wired up cleanly.
--
-- Renames:
--   proposed_skills      → skill_candidates       (table)
--   applied_at_ms        → decision_at_ms         (now generic across add/merge/discard)
--   applied_path         → add_path               (only meaningful for decision='add')
--
-- New columns:
--   decision        TEXT      'add' | 'merge' | 'discard' | NULL=pending
--   merged_into_id  INTEGER   FK to skill_candidates(id), set when decision='merge'
--   triggers        TEXT      JSON array of strings — AutoSkill τ
--   tags            TEXT      JSON array of strings — AutoSkill γ
--   examples        TEXT      JSON array of {input, output} — AutoSkill ξ
--   version         TEXT      semver-ish ("v0.1.0") — AutoSkill v; merge bumps the patch
--
-- Backfill: any pre-migration row with decision_at_ms (= the old
-- applied_at_ms) set was the previous code's "applied" path, which
-- corresponds to AutoSkill action 'add'. Pending rows stay NULL.
--
-- Indexes are dropped+recreated under the new table name. SQLite
-- silently rewrites the partial index predicate to use the renamed
-- column, but the index identifier stays as-is, which makes
-- .schema confusing — the rebuild keeps the schema readable.
--
-- The pre-existing superseded_by_id column (declared in migration
-- 018 but never written by any code path, and a dead variant of the
-- merge-target idea this migration introduces cleanly) cannot be
-- dropped in place because SQLite forbids DROP COLUMN on a column
-- referenced by a self-FK constraint. Leaving it as dead schema
-- here; a follow-up table-recreate migration can remove it once
-- it's worth the churn.

ALTER TABLE proposed_skills RENAME TO skill_candidates;

ALTER TABLE skill_candidates RENAME COLUMN applied_at_ms TO decision_at_ms;
ALTER TABLE skill_candidates RENAME COLUMN applied_path  TO add_path;

ALTER TABLE skill_candidates ADD COLUMN decision       TEXT;
ALTER TABLE skill_candidates ADD COLUMN merged_into_id INTEGER
    REFERENCES skill_candidates(id) ON DELETE SET NULL;
ALTER TABLE skill_candidates ADD COLUMN triggers TEXT;
ALTER TABLE skill_candidates ADD COLUMN tags     TEXT;
ALTER TABLE skill_candidates ADD COLUMN examples TEXT;
ALTER TABLE skill_candidates ADD COLUMN version  TEXT;

UPDATE skill_candidates
   SET decision = 'add'
 WHERE decision_at_ms IS NOT NULL;

DROP INDEX IF EXISTS idx_proposed_skills_name;
DROP INDEX IF EXISTS idx_proposed_skills_applied;

CREATE INDEX idx_skill_candidates_name ON skill_candidates(skill_name);
CREATE INDEX idx_skill_candidates_decided
    ON skill_candidates(decision_at_ms DESC)
    WHERE decision_at_ms IS NOT NULL;

UPDATE meta SET value='21' WHERE key='schema_version';
