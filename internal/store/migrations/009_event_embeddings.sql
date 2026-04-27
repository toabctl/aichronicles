-- 009_event_embeddings.sql
--
-- Adds an event_embeddings table for semantic search. One row per
-- event with a vector representation of its content_text, packed
-- as little-endian float32 in the `vec` BLOB column.
--
-- We keep the embedding store deliberately minimal:
--
--   - One row per event_id (PK + FK CASCADE), so deleting an event
--     drops its embedding automatically. Same lifecycle the
--     extractions table already follows.
--
--   - `model` + `dim` are stored alongside the BLOB so the search
--     path can refuse to compare vectors from incompatible models
--     and the user can mix embeddings during a model upgrade
--     (re-embed at leisure; old rows survive until cleanup).
--
--   - Pure BLOB column, no sqlite-vec extension. Our SQLite driver
--     is modernc.org/sqlite (pure-Go, no extension support); cosine
--     similarity runs in Go at search time. A future optimization
--     could swap in a CGo driver and a vec0 virtual table for
--     indexed kNN once full-scan latency bites — until then the
--     plain BLOB is enough for the user's data scale (~100k events
--     * 1.5KB ≈ 150MB resident).
CREATE TABLE event_embeddings (
    event_id      TEXT PRIMARY KEY REFERENCES events(event_id) ON DELETE CASCADE,
    model         TEXT NOT NULL,         -- e.g. "text-embedding-3-small"
    dim           INTEGER NOT NULL,      -- vector dimensionality (e.g. 1536)
    vec           BLOB NOT NULL,         -- little-endian float32, dim*4 bytes
    created_at_ms INTEGER NOT NULL
);

-- Help the "which events still need embedding?" query stay cheap
-- as the table grows. The model column is part of the key so a
-- mid-flight model swap (re-embed with text-embedding-3-large
-- without dropping old rows) gets queried efficiently too.
CREATE INDEX idx_event_embeddings_model ON event_embeddings(model);

INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', '9');
