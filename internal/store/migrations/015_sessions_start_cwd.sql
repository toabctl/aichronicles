-- Add sessions.start_cwd as a first-class column.
--
-- sessions.cwd is maintained by the AFTER-INSERT trigger from
-- migration 001 to track the LATEST non-null cwd seen — useful when
-- the user cd's mid-session and we want to know "where did this
-- session end up?" But several callers actually want the START
-- cwd: the directory the session was launched in. They reconstruct
-- it via expensive correlated subqueries:
--
--   * LoadSessionStartCwd: SELECT MIN(ts_source_ms) WHERE cwd NOT NULL
--   * LoadProjectAggregates: a 9-line CTE re-deriving the same value
--
-- Both are slow at scale and brittle. Materializing start_cwd as a
-- column collapses both query paths to a column read. The existing
-- sessions.cwd column keeps its "last non-null" semantics — callers
-- that genuinely want that (the resume button when the user cd'd
-- away and back) keep working unchanged.
--
-- Nullable on purpose: a session with zero events has no cwd to
-- carry. Callers fall back to s.cwd or hide the dependent UI when
-- start_cwd IS NULL.

ALTER TABLE sessions ADD COLUMN start_cwd TEXT;

-- Backfill: first non-null cwd per session in event-time order.
-- Tie-break on rowid (matching the LoadSessionStartCwd query)
-- so two events with the same ts_source_ms resolve deterministically.
UPDATE sessions
   SET start_cwd = (
       SELECT cwd FROM events
        WHERE session_id = sessions.id AND cwd IS NOT NULL
        ORDER BY ts_source_ms ASC, rowid ASC
        LIMIT 1
   );

-- Trigger to keep start_cwd current. Fires only on the FIRST
-- non-null cwd: subsequent cwd changes (the user cd'd into a
-- subdir mid-session) leave start_cwd alone. This is the property
-- the existing sessions_agg_ai trigger does NOT provide — that one
-- always overwrites cwd with the latest seen.
CREATE TRIGGER sessions_start_cwd_ai
AFTER INSERT ON events
WHEN new.cwd IS NOT NULL
BEGIN
    UPDATE sessions
       SET start_cwd = new.cwd
     WHERE id = new.session_id AND start_cwd IS NULL;
END;

UPDATE meta SET value='15' WHERE key='schema_version';
