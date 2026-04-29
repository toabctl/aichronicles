-- Migration 022: drop the unused superseded_by_id column.
--
-- superseded_by_id was declared in migration 018 ("a later proposal
-- with the same skill_name marks the prior row superseded_by_id")
-- but no code path ever wrote to it. Migration 021 introduced the
-- AutoSkill (Yang et al., 2026 — arXiv:2603.01145) lifecycle with
-- merged_into_id playing the actual "this row was replaced by a
-- later one" role; superseded_by_id has been a dead column ever
-- since. Migration 021 left it in place because SQLite's DROP
-- COLUMN refuses to drop a column referenced by its own foreign
-- key constraint (superseded_by_id has REFERENCES skill_candidates
-- (id)) and table-recreates require care around self-FKs.
--
-- This migration takes that care: defer_foreign_keys=ON inside the
-- migration transaction so the merged_into_id self-FK on the new
-- table doesn't fire mid-INSERT, while the existing data is copied
-- across with IDs preserved.
--
-- The migration runner wraps this whole script in a transaction
-- (see store.applyMigration); defer_foreign_keys auto-resets on
-- commit per SQLite docs, so callers see the normal foreign_keys=ON
-- semantics afterwards.

PRAGMA defer_foreign_keys = ON;

CREATE TABLE skill_candidates_new (
    id              INTEGER PRIMARY KEY,
    llm_output_id   INTEGER NOT NULL REFERENCES llm_outputs(id) ON DELETE CASCADE,
    skill_name      TEXT NOT NULL,
    proposed_at_ms  INTEGER NOT NULL,
    decision_at_ms  INTEGER,
    add_path        TEXT,
    decision        TEXT,
    merged_into_id  INTEGER REFERENCES skill_candidates_new(id) ON DELETE SET NULL,
    triggers        TEXT,
    tags            TEXT,
    examples        TEXT,
    version         TEXT,
    UNIQUE(llm_output_id, skill_name)
);

INSERT INTO skill_candidates_new
       (id, llm_output_id, skill_name, proposed_at_ms,
        decision_at_ms, add_path, decision, merged_into_id,
        triggers, tags, examples, version)
SELECT  id, llm_output_id, skill_name, proposed_at_ms,
        decision_at_ms, add_path, decision, merged_into_id,
        triggers, tags, examples, version
  FROM skill_candidates;

DROP TABLE skill_candidates;

ALTER TABLE skill_candidates_new RENAME TO skill_candidates;

-- Indexes: the originals from migration 021 lived on the dropped
-- table; recreate them on the new one. CREATE INDEX is idempotent
-- via IF NOT EXISTS so a partially-applied state would still
-- converge.
CREATE INDEX IF NOT EXISTS idx_skill_candidates_name
    ON skill_candidates(skill_name);
CREATE INDEX IF NOT EXISTS idx_skill_candidates_decided
    ON skill_candidates(decision_at_ms DESC)
    WHERE decision_at_ms IS NOT NULL;

UPDATE meta SET value='22' WHERE key='schema_version';
