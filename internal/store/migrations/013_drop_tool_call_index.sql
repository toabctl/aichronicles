-- Drop idx_events_tool_call.
--
-- Migration 011 left this index in place under the assumption that
-- a future tool_use ↔ tool_result pairing feature would need it.
-- The user has confirmed the feature isn't on the near-term roadmap,
-- and write amplification on every event ingest is paid for nothing
-- in the meantime. Easy to add back when the feature lands.

DROP INDEX IF EXISTS idx_events_tool_call;

UPDATE meta SET value='13' WHERE key='schema_version';
