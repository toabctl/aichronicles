-- Migration 024: skill_candidates.kind — pattern vs pitfall.
--
-- Until now every induced / proposed skill encoded the same shape:
-- "when condition X fires, do procedure Y" — the success-pattern
-- form. EvoSkill (Sentient/VT, 2026 — arXiv:2603.02766) and EvoSC
-- (2602.01966) both argue that failure-driven induction is the
-- complementary half: extract a "when X is about to fail, AVOID
-- doing Y" rule from sessions where things went wrong, and stash
-- that as a distinct skill kind.
--
-- aichronicles already has the data: session_outcomes flags
-- failure_likely sessions, and the propose system prompt rule 13
-- already invites the LLM to consider prevention skills. What was
-- missing was a label on the candidate so we can DISTINGUISH a
-- pattern (do-this) skill from a pitfall (avoid-this) skill across
-- the lifecycle — the metrics, the merge gate, the SKILL.md
-- frontmatter, and the eventual evolve / discard sweeps all want
-- to treat the two differently.
--
-- Default 'pattern' covers every legacy row: the existing
-- candidates were extracted from successful or mixed sessions
-- under the success-pattern frame, and labelling them retro-
-- actively as 'pattern' is faithful to what the LLM emitted.

ALTER TABLE skill_candidates ADD COLUMN kind TEXT NOT NULL DEFAULT 'pattern';

UPDATE meta SET value='24' WHERE key='schema_version';
