-- Race-free ingest_seq allocation.
--
-- Migration 001 declared raw_envelopes.ingest_seq as a NOT NULL UNIQUE
-- INTEGER, and IngestEnvelope used a sub-query to compute the next
-- value:
--
--   INSERT OR IGNORE INTO raw_envelopes(event_id, ingest_seq, ...)
--     VALUES (?, (SELECT COALESCE(MAX(ingest_seq), 0) + 1 FROM raw_envelopes), ...)
--
-- Two concurrent transactions can both compute the same MAX+1 inside
-- their own snapshot. The second INSERT hits the UNIQUE constraint
-- and INSERT OR IGNORE rolls it back silently — the caller sees
-- RowsAffected=0 and returns deduped=true even though no real
-- duplicate existed. The event is dropped invisibly.
--
-- This migration introduces a tiny sequence table whose UPDATE
-- RETURNING grabs the SQLite write lock atomically. Concurrent
-- transactions serialize on this UPDATE; each gets a unique value.
-- The seed pulls forward whatever MAX the existing rows have so
-- post-migration the next ingest_seq is one above the highest
-- already-stored value.

CREATE TABLE seq (
    name        TEXT PRIMARY KEY,
    next_value  INTEGER NOT NULL
);

INSERT INTO seq(name, next_value)
    SELECT 'ingest_seq', COALESCE(MAX(ingest_seq), 0) + 1
      FROM raw_envelopes;

UPDATE meta SET value='8' WHERE key='schema_version';
