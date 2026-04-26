-- Subagent threading.
--
-- Claude Code's SubagentStart / SubagentStop already round-trip
-- through internal/cli/assemble.go's hookKindMap as canonical
-- subagent_start / subagent_stop event kinds, but the events are
-- flat: there's no way to ask "what did the planner subagent do
-- last Tuesday" without scanning every event in the session.
--
-- This migration adds two nullable columns to events:
--
--   subagent_id    Per-session subagent identifier from the
--                  hook payload's `agent_id` field. Top-level
--                  events run by the main agent leave this NULL.
--                  Two events with the same (session_id,
--                  subagent_id) belong to the same subagent
--                  thread.
--
--   subagent_type  The role label the host emits — typically
--                  "planner" / "researcher" / etc. Absent for
--                  hosts that don't expose one; informational
--                  only (subagent_id is the identity key).
--
-- Linkage to a causal parent event id is intentionally NOT modelled
-- here. (session_id, subagent_id) is enough to query "events run
-- inside this thread"; tracing the main-agent event that spawned
-- the thread would require host metadata we don't have today.
--
-- A partial index on (session_id, subagent_id, ts_source_ms) WHERE
-- subagent_id IS NOT NULL keeps the index narrow — the common case
-- (top-level events) doesn't pay the storage cost.

ALTER TABLE events ADD COLUMN subagent_id TEXT;
ALTER TABLE events ADD COLUMN subagent_type TEXT;

CREATE INDEX idx_events_subagent
    ON events(session_id, subagent_id, ts_source_ms)
    WHERE subagent_id IS NOT NULL;

UPDATE meta SET value='7' WHERE key='schema_version';
