-- Round 12: drop the embedding system entirely.
--
-- event_embeddings was added in migration 009 to back semantic
-- search (CLI / MCP), workflow task_shape ranking, and the
-- summarize candidate-prior rerank pass. After real-use review
-- the embedding layer was an optional enhancement, not a
-- foundation:
--
--   * the core self-improvement loop (summarize → induction →
--     facts) never read or wrote embeddings;
--   * SQLite FTS5 with porter stemming handled the bulk of
--     keyword retrieval needs honestly well at personal-use
--     scale;
--   * the candidate-prior rerank had a recency-only fallback
--     that was the correct pre-Round-3 default;
--   * embeddings cost an extra provider dependency (OpenAI,
--     since Anthropic doesn't expose hosted embeddings).
--
-- Dropping the table reclaims storage and removes a maintenance
-- surface (provider adapter, embedder worker, three retrieval
-- handlers). The data is irrecoverable; the user explicitly
-- opted into "ignore backwards compat" before this round.
--
-- event_embeddings carried an FK INTO events.event_id but no
-- other table referenced it, so the drop is clean — no cascade
-- fallout, no orphan rows.

DROP TABLE IF EXISTS event_embeddings;

UPDATE meta SET value='20' WHERE key='schema_version';
