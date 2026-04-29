-- Migration 025: episodes — instance-specific contextually-bound
-- retrievable units of session experience.
--
-- Pink et al. (2026 — arXiv:2502.06975, "Episodic Memory is the
-- Missing Piece for Long-Term LLM Agents") argue that the field's
-- universal gap is the *episodic* memory slot: a chunk of related
-- events with a contextual signature (cwd, intent, time bracket)
-- that an agent can retrieve instance-by-instance. RAG/in-context/
-- parametric memory each cover at most three of the five episodic
-- properties (long-term, explicit, single-shot, instance-specific,
-- contextually-related); none covers all five.
--
-- aichronicles ingests events (too granular) and aggregates them
-- into sessions (too coarse and not contextually-bound — a long
-- session can span multiple intents and cwds). The episodes table
-- sits between: a contiguous run of events sharing a cwd and a
-- coherent intent, bounded by idle gaps. Populated by a segmenter
-- that walks each session's ordered events; per-session ordinal
-- gives a stable retrieval handle ("show me the third episode in
-- session abc12345").
--
-- Schema:
--   id              autoincrement PK.
--   session_id      FK to sessions(id), CASCADE on delete.
--   ordinal         1-based position within the parent session
--                   (a session always has at least episode 1).
--   started_at_ms   first event's ts_source_ms in this episode.
--   ended_at_ms     last event's ts_source_ms in this episode.
--   cwd             the running cwd for this episode (NULL if
--                   no event in the episode carried a cwd).
--   intent_summary  short prose summary derived from the first
--                   user_prompt in the episode (empty when no
--                   user_prompt landed).
--   event_count     how many events ended up in this episode.
--   first_event_id  the chronologically-first event_id in the
--                   episode — gives a deterministic anchor for
--                   future re-segmentation against the same data.
--
-- (session_id, ordinal) is naturally unique. Enforce via UNIQUE.
-- The segmenter is idempotent: re-running on the same events
-- produces the same boundaries; a re-population path will use
-- DELETE-then-INSERT under a transaction rather than UPDATE.

CREATE TABLE episodes (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal         INTEGER NOT NULL,
    started_at_ms   INTEGER NOT NULL,
    ended_at_ms     INTEGER NOT NULL,
    cwd             TEXT,
    intent_summary  TEXT NOT NULL DEFAULT '',
    event_count     INTEGER NOT NULL,
    first_event_id  TEXT NOT NULL REFERENCES events(event_id) ON DELETE CASCADE,
    UNIQUE(session_id, ordinal)
);

-- Read paths: "all episodes for a session, in order" (ordinal asc),
-- plus "all episodes touching a given cwd in a window" for the
-- future cwd-scoped retrieval the synthesis recommends.
CREATE INDEX idx_episodes_session_ordinal
    ON episodes(session_id, ordinal);
CREATE INDEX idx_episodes_cwd_started
    ON episodes(cwd, started_at_ms DESC)
    WHERE cwd IS NOT NULL;

UPDATE meta SET value='25' WHERE key='schema_version';
