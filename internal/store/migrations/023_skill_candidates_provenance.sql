-- Migration 023: tamper-evidence for added skills (SSGM provenance hash).
--
-- SSGM (Lam et al., 2026 — arXiv:2603.11768) prescribes that
-- governance for an evolving memory bank requires provenance and
-- integrity primitives, not just retrieval scoring. The lightest-
-- weight version aichronicles can ship is a content hash of the
-- rendered SKILL.md body, captured at write time and stored on the
-- skill_candidates row alongside add_path.
--
-- Drift detection becomes possible: a later sweep reads the
-- on-disk SKILL.md, hashes its body, and compares against
-- add_body_sha256 — a mismatch flags either a hand-edit, an
-- external rewrite, or filesystem corruption. Without the column
-- the lifecycle index has no way to tell "this skill is what we
-- wrote" from "this skill is what someone else wrote afterwards."
--
-- NULL when decision != 'add' (no on-disk file to hash) or when
-- the skill was added before this migration.

ALTER TABLE skill_candidates ADD COLUMN add_body_sha256 TEXT;

UPDATE meta SET value='23' WHERE key='schema_version';
