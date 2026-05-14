-- Migration 028: enforce merge-target invariants for skill_candidates.
--
-- AutoSkill's merge action records `merged_into_id` pointing at the
-- candidate row whose on-disk SKILL.md was rewritten to fold the
-- source's content in. Migration 021 wired the field; nothing
-- enforced two invariants the audit at 2026-05-14 surfaced:
--
--   1. merged_into_id must point at a row whose decision is 'add'.
--      A merge target that's still pending, already discarded, or
--      itself a merge claim represents an inconsistent edit
--      history — the on-disk SKILL.md the user wrote into doesn't
--      correspond to the row the schema says it does.
--
--   2. An 'add' row that's the target of a merge can't transition
--      away from 'add'. Discarding A after B was merged into A
--      would leave B's merged_into_id pointing at a no-longer-live
--      row. ON DELETE SET NULL handles physical row deletion; it
--      doesn't fire on a decision-field state change because the
--      row sticks around.
--
-- SQLite CHECK constraints can only reference the row being
-- written, not the rest of the table. The invariants need
-- triggers. Using BEFORE ... RAISE(ABORT, ...) so the violation
-- aborts the transaction with a SQLITE_CONSTRAINT_TRIGGER and a
-- readable message the Go layer surfaces to the user.
--
-- The hand-authored-merge sentinel (merged_into_id = NULL when a
-- candidate is folded into a pre-aichronicles SKILL.md that has no
-- candidate row) is preserved: both triggers check for NOT NULL
-- before validating the target.

CREATE TRIGGER skill_candidates_merge_target_must_be_added_ins
BEFORE INSERT ON skill_candidates
FOR EACH ROW
WHEN NEW.merged_into_id IS NOT NULL
 AND (SELECT decision FROM skill_candidates WHERE id = NEW.merged_into_id) IS NOT 'add'
BEGIN
    SELECT RAISE(ABORT, 'merged_into_id must point at a candidate with decision=add');
END;

CREATE TRIGGER skill_candidates_merge_target_must_be_added_upd
BEFORE UPDATE OF merged_into_id ON skill_candidates
FOR EACH ROW
WHEN NEW.merged_into_id IS NOT NULL
 AND (SELECT decision FROM skill_candidates WHERE id = NEW.merged_into_id) IS NOT 'add'
BEGIN
    SELECT RAISE(ABORT, 'merged_into_id must point at a candidate with decision=add');
END;

CREATE TRIGGER skill_candidates_no_orphan_merge_target
BEFORE UPDATE OF decision ON skill_candidates
FOR EACH ROW
WHEN OLD.decision = 'add' AND NEW.decision IS NOT 'add'
 AND EXISTS (SELECT 1 FROM skill_candidates WHERE merged_into_id = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'cannot change decision: another candidate has this row as its merge target');
END;

UPDATE meta SET value='28' WHERE key='schema_version';
