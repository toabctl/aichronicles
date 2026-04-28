-- Drop unused indexes the audit identified.
--
-- idx_raw_ts_server (raw_envelopes.ts_server_ms) was created in
-- migration 001 but no store-layer query filters or sorts on
-- ts_server_ms. The live-feed and prune paths both use ingest_seq
-- and ts_source_ms respectively. Pure write amplification on every
-- ingest with no read benefit.
--
-- idx_events_tool_call (events.tool_call_id, partial) is left in
-- place pending an explicit decision: tool_call_id is written but
-- not yet queried. It is the obvious key for a future tool_use ↔
-- tool_result pairing feature and dropping it now would require
-- backfilling the index later. Worth keeping.
DROP INDEX IF EXISTS idx_raw_ts_server;

UPDATE meta SET value='11' WHERE key='schema_version';
