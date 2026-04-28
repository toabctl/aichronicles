-- Promote `transport` from a JSON-blob field to a first-class column.
--
-- search.go's deduped path needs to break ties between hook-sourced
-- and import-sourced rows for the same logical turn — hook wins so
-- live data takes precedence over a later transcript import. Today
-- it does that via:
--
--   CASE WHEN json_extract(r.envelope_json, '$.transport') = 'hook'
--        THEN 0 ELSE 1 END AS transport_rank
--
-- That parses the entire envelope JSON for every matched row in the
-- dedup CTE — fine at thousands of events, wasteful at hundreds of
-- thousands. Promoting the field to a column makes the dedup a
-- direct comparison on an indexed-friendly value.
--
-- Backfill via SQLite's json_extract over raw_envelopes — the only
-- authoritative source (Envelope.Transport is set on every ingest
-- path: hook assemblers, import-claude, import-gemini, import-jsonl).
-- Rows with no transport in their JSON (legacy envelopes, third-party
-- bridges) get '' so the column is never NULL — keeps the search
-- comparison straightforward (column = 'hook' is true XOR false).

ALTER TABLE events ADD COLUMN transport TEXT NOT NULL DEFAULT '';

UPDATE events
   SET transport = COALESCE(
       (SELECT json_extract(r.envelope_json, '$.transport')
          FROM raw_envelopes r
         WHERE r.event_id = events.event_id),
       ''
   );

-- Partial index on transport='hook' rows powers the dedup CASE
-- expression: hook events are usually the minority (one per
-- session, vs many per session for imports), so the partial form
-- is small. Searches without dedup don't need the index.
CREATE INDEX idx_events_transport_hook
    ON events(event_id) WHERE transport = 'hook';

UPDATE meta SET value='12' WHERE key='schema_version';
