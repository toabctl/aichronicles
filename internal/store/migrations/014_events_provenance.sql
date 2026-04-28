-- Persist provenance fields source_agent_version and host as
-- first-class columns on events.
--
-- The audit found both fields accepted on the wire (api/openapi.yaml
-- documents them, Envelope declares them, import_claude actively
-- populates SourceAgentVersion from the transcript header) but
-- silently dropped — the daemon and store never wrote them anywhere.
-- "We have the data, we throw it away" is the §7 (CLAUDE.md) anti-
-- pattern: persisting them costs little and unlocks queries like
-- "is this session from a known-good Claude Code build?" that the
-- data already supports.
--
-- NOT NULL DEFAULT '' so the dedup / ranking expressions in search
-- never need to worry about NULL coalescing — same shape as the
-- transport column added in migration 012.

ALTER TABLE events ADD COLUMN source_agent_version TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN host                 TEXT NOT NULL DEFAULT '';

UPDATE events
   SET source_agent_version = COALESCE(
       (SELECT json_extract(r.envelope_json, '$.source_agent_version')
          FROM raw_envelopes r
         WHERE r.event_id = events.event_id),
       ''
   ),
       host = COALESCE(
       (SELECT json_extract(r.envelope_json, '$.host')
          FROM raw_envelopes r
         WHERE r.event_id = events.event_id),
       ''
   );

UPDATE meta SET value='14' WHERE key='schema_version';
