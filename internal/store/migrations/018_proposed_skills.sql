-- Track the lifecycle of every skill the LLM proposes.
--
-- Until this migration, the propose → apply flow is open-loop: the
-- LLM emits N skill proposals, the user runs `propose apply --skill
-- <name>` for the ones they like, the SKILL.md lands on disk, and
-- nothing further is recorded. Down-stream propose runs see "skills
-- installed" and "skills invoked" but not the provenance — was a
-- skill on disk authored by hand or scaffolded from a prior
-- proposal? Was a proposed skill ever applied at all? Did applied
-- proposals get used afterward?
--
-- That gap is the load-bearing missing piece in the AWM (Agent
-- Workflow Memory) loop: without proposal lifecycle data the system
-- cannot tell its own past suggestions from random skill files, and
-- cannot evaluate whether its proposals actually helped.
--
-- This table is the index. Every (llm_output_id, skill_name) pair
-- the propose / induction / skill-revision paths emit gets one row
-- on persistence. apply updates applied_at_ms + applied_path. A
-- later proposal with the same skill_name marks the prior row
-- superseded_by_id (newest wins for "which proposal birthed the
-- skill on disk today"). Each transition is monotonic in
-- proposed_at_ms so the historical sequence stays reconstructable.
--
-- proposed_at_ms is NOT NULL — every row was proposed at some
-- moment. applied_at_ms IS NULL when the proposal was never
-- accepted; that's the abandonment-rate signal — proposals the LLM
-- thought were worthwhile but the user did not.
--
-- llm_output_id is the FK to the propose / induction llm_outputs
-- row. ON DELETE CASCADE so cleanup of cache entries flows through.
-- skill_name is kebab-case canonical (matches the proposal schema's
-- ^[a-z][a-z0-9-]*$ pattern). UNIQUE(llm_output_id, skill_name) is
-- the natural key — one proposal output may name a skill at most
-- once.

CREATE TABLE proposed_skills (
    id                INTEGER PRIMARY KEY,
    llm_output_id     INTEGER NOT NULL REFERENCES llm_outputs(id) ON DELETE CASCADE,
    skill_name        TEXT NOT NULL,
    proposed_at_ms    INTEGER NOT NULL,
    applied_at_ms     INTEGER,
    applied_path      TEXT,
    superseded_by_id  INTEGER REFERENCES proposed_skills(id) ON DELETE SET NULL,
    UNIQUE(llm_output_id, skill_name)
);

-- "Find every proposal of skill X across history" + lookup-by-name
-- joins from extractions(skill_load).value.
CREATE INDEX idx_proposed_skills_name ON proposed_skills(skill_name);

-- Partial index for "show me everything that's actually on disk."
-- The applied subset is small (~tens of rows) — full-table scans
-- on the unfiltered column would still be fast at our scale, but
-- partial keeps the query plan obviously right under EXPLAIN.
CREATE INDEX idx_proposed_skills_applied
    ON proposed_skills(applied_at_ms DESC)
    WHERE applied_at_ms IS NOT NULL;

UPDATE meta SET value='18' WHERE key='schema_version';
