-- Schema v1: full aichronicles storage, all layers.
-- Raw envelopes are sacred; everything else is derivable from them.

-- Schema versioning / metadata.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Layer 1: raw envelopes — append-only, content never rewritten.
CREATE TABLE raw_envelopes (
    event_id           TEXT PRIMARY KEY,        -- UUIDv7, client-assigned
    ingest_seq         INTEGER NOT NULL UNIQUE, -- monotonic server-side order
    source_agent       TEXT NOT NULL,
    source_session_id  TEXT NOT NULL,
    ts_source_ms       INTEGER NOT NULL,        -- client clock
    ts_server_ms       INTEGER NOT NULL,        -- daemon receipt
    envelope_json      TEXT NOT NULL
);
CREATE INDEX idx_raw_source_session ON raw_envelopes(source_agent, source_session_id);
CREATE INDEX idx_raw_ts_server      ON raw_envelopes(ts_server_ms);

-- Layer 2: sessions — one row per conversation, trigger-maintained aggregates.
CREATE TABLE sessions (
    id                 TEXT PRIMARY KEY,        -- UUIDv5(source_agent + ":" + source_session_id)
    source_agent       TEXT NOT NULL,
    source_session_id  TEXT NOT NULL,
    cwd                TEXT,
    started_at_ms      INTEGER,
    ended_at_ms        INTEGER,
    event_count        INTEGER NOT NULL DEFAULT 0,
    UNIQUE(source_agent, source_session_id)
);

-- Layer 2: events — typed projection of raw envelopes.
CREATE TABLE events (
    event_id       TEXT PRIMARY KEY REFERENCES raw_envelopes(event_id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL     REFERENCES sessions(id)            ON DELETE CASCADE,
    source_agent   TEXT NOT NULL,
    kind           TEXT NOT NULL,
    role           TEXT,
    ts_source_ms   INTEGER NOT NULL,
    cwd            TEXT,
    tool_name      TEXT,
    tool_call_id   TEXT,
    content_text   TEXT
);
CREATE INDEX idx_events_session_ts ON events(session_id, ts_source_ms);
CREATE INDEX idx_events_kind       ON events(kind);
CREATE INDEX idx_events_tool_call  ON events(tool_call_id) WHERE tool_call_id IS NOT NULL;

-- Trigger: keep sessions aggregates in sync with events.
CREATE TRIGGER sessions_agg_ai AFTER INSERT ON events BEGIN
    UPDATE sessions
       SET event_count   = event_count + 1,
           started_at_ms = MIN(COALESCE(started_at_ms, new.ts_source_ms), new.ts_source_ms),
           ended_at_ms   = MAX(COALESCE(ended_at_ms,   0),                new.ts_source_ms),
           cwd           = COALESCE(new.cwd, cwd)
     WHERE id = new.session_id;
END;

-- Layer 3: FTS5 over events.content_text — contentless, porter+unicode.
CREATE VIRTUAL TABLE events_fts USING fts5(
    content_text,
    content='events',
    content_rowid='rowid',
    tokenize='porter unicode61'
);
CREATE TRIGGER events_fts_ai AFTER INSERT ON events BEGIN
    INSERT INTO events_fts(rowid, content_text) VALUES (new.rowid, new.content_text);
END;
CREATE TRIGGER events_fts_ad AFTER DELETE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, content_text) VALUES ('delete', old.rowid, old.content_text);
END;
CREATE TRIGGER events_fts_au AFTER UPDATE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, content_text) VALUES ('delete', old.rowid, old.content_text);
    INSERT INTO events_fts(rowid, content_text) VALUES (new.rowid, new.content_text);
END;

-- Layer 4: polymorphic extractions — urls, file paths, shell commands, …
CREATE TABLE extractions (
    id           INTEGER PRIMARY KEY,
    event_id     TEXT NOT NULL REFERENCES events(event_id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL REFERENCES sessions(id)     ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    value        TEXT NOT NULL,
    extra_json   TEXT
);
CREATE INDEX idx_extractions_session_kind ON extractions(session_id, kind);
CREATE INDEX idx_extractions_kind_value   ON extractions(kind, value);

-- Final step: record this migration.
INSERT INTO meta(key, value) VALUES ('schema_version', '1');
