-- Materialize sessions.first_prompt_text and sessions.summary_topic.
--
-- Eight callers across cli/mcp/web/store re-derive "the first
-- user_prompt content_text for this session" via the same
-- correlated subquery; four more re-derive "the topic field from
-- the latest summary body" via a json_extract subquery. Every
-- session-list render pays the cost. The audit identified these
-- as the highest-volume duplications.
--
-- Materialising both as columns on sessions, kept current by
-- AFTER INSERT triggers, collapses the per-row subqueries to a
-- column read. The audit's task #24 (summary topic JSON parse)
-- and task #21 (the unified session-list reader) both become
-- trivial against this shape — callers either read the column or
-- they don't, no more parametric subquery composition.
--
-- Both columns are nullable: a session with no user_prompt yet,
-- or no cached summary yet, has nothing to project. Callers
-- fall back to "(no prompt)" / "(no summary)" the same way they
-- did before.
--
-- Triggers:
--
--   sessions_first_prompt_ai: fires on every event INSERT, updates
--     first_prompt_text only when it's currently NULL and the new
--     event is a user_prompt with non-null content. Sticky on the
--     first user_prompt seen.
--
--   sessions_summary_topic_ai: fires on every llm_outputs INSERT
--     where kind='summary', overwrites summary_topic from
--     json_extract(body, '$.topic'). Always reflects the LATEST
--     summary so the rolling re-summarize-after-edit case works.

ALTER TABLE sessions ADD COLUMN first_prompt_text TEXT;
ALTER TABLE sessions ADD COLUMN summary_topic     TEXT;

-- Backfill first_prompt_text from the existing correlated subquery
-- shape (oldest user_prompt with non-null content_text per session).
UPDATE sessions
   SET first_prompt_text = (
       SELECT content_text FROM events
        WHERE session_id = sessions.id
          AND kind = 'user_prompt'
          AND content_text IS NOT NULL
        ORDER BY ts_source_ms ASC, rowid ASC
        LIMIT 1
   );

-- Backfill summary_topic from the latest kind=summary llm_outputs
-- per session, parsed via SQLite's built-in json_extract. Guarded
-- with json_valid so a malformed body (legacy prose, partial write)
-- doesn't crash the migration — it just leaves summary_topic NULL
-- for that session, which callers already handle.
UPDATE sessions
   SET summary_topic = (
       SELECT CASE WHEN json_valid(body)
                   THEN json_extract(body, '$.topic')
                   ELSE NULL END
         FROM llm_outputs
        WHERE session_id = sessions.id AND kind = 'summary'
        ORDER BY created_at_ms DESC
        LIMIT 1
   );

-- First-prompt trigger: re-derive from the canonical
-- "earliest-timestamp user_prompt with non-null content" rule on
-- every user_prompt insert. This pays a one-row indexed lookup per
-- write but matches the previous correlated-subquery semantics
-- exactly — including the edge case where imports backfill an
-- older user_prompt after a newer one already landed via the live
-- hook. A simpler "set when NULL" trigger would silently lose
-- that case (the new earlier prompt arrives, but first_prompt_text
-- stays anchored to the later live one). The full-rederive cost
-- is paid on writes (cheap, indexed) so reads stay free.
CREATE TRIGGER sessions_first_prompt_ai
AFTER INSERT ON events
WHEN new.kind = 'user_prompt' AND new.content_text IS NOT NULL
BEGIN
    UPDATE sessions
       SET first_prompt_text = (
           SELECT e.content_text FROM events e
            WHERE e.session_id = new.session_id
              AND e.kind = 'user_prompt'
              AND e.content_text IS NOT NULL
            ORDER BY e.ts_source_ms ASC, e.rowid ASC
            LIMIT 1
       )
     WHERE id = new.session_id;
END;

-- Latest-wins summary-topic trigger. Guarded by json_valid so a
-- malformed body (the test fixtures use plain strings; legacy
-- prose summaries from before record_summary tool calls landed)
-- doesn't break the INSERT. Bodies that don't parse leave
-- summary_topic NULL — same fallback callers handle today via
-- extractSummaryTopic returning empty string.
CREATE TRIGGER sessions_summary_topic_ai
AFTER INSERT ON llm_outputs
WHEN new.kind = 'summary' AND new.session_id IS NOT NULL
BEGIN
    UPDATE sessions
       SET summary_topic = CASE
           WHEN json_valid(new.body)
           THEN json_extract(new.body, '$.topic')
           ELSE NULL
       END
     WHERE id = new.session_id;
END;

UPDATE meta SET value='16' WHERE key='schema_version';
