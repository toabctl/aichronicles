-- Migration 026: realign episodes' cwd-scoped index to match the
-- ORDER BY in store.FindEpisodes.
--
-- Migration 025 created `idx_episodes_cwd_started ON episodes(cwd,
-- started_at_ms DESC) WHERE cwd IS NOT NULL`, anticipating a
-- "started_at_ms-ordered" cwd-scoped retrieval that ended up not
-- being the actual sort key. FindEpisodes orders by `ended_at_ms
-- DESC, id DESC`, so the started-anchored index can only help with
-- the cwd= equality predicate; SQLite still has to materialise the
-- matching set and re-sort it. For a recall query that's
-- consistently LIMIT-bounded ("most recent N episodes in this
-- project"), that's a hot-path inefficiency.
--
-- Drop the old index and create one anchored on `ended_at_ms DESC`,
-- with `id DESC` baked in so the planner can satisfy ORDER BY...
-- LIMIT directly from the index without a sort step.
--
-- The compound index is still partial (WHERE cwd IS NOT NULL): rows
-- with NULL cwd never participate in cwd= filters, so excluding them
-- keeps the index small.
DROP INDEX IF EXISTS idx_episodes_cwd_started;
CREATE INDEX idx_episodes_cwd_ended
    ON episodes(cwd, ended_at_ms DESC, id DESC)
    WHERE cwd IS NOT NULL;

UPDATE meta SET value='26' WHERE key='schema_version';
