# Schema reference

Every SQL migration shipped in the binary, in apply order. Auto-generated from `internal/store/migrations/` via `make docs-schema`; edit the SQL files, not this page.

Schema design rationale lives at [../explanation/architecture.md](../explanation/architecture.md).

## `001_initial.sql`

```sql
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
```

## `002_llm_outputs.sql`

```sql
-- Block B storage: outputs from LLM calls (summarize, reflect, propose).
--
-- Why a dedicated table, not another `extractions` row: extractions
-- are small, high-cardinality, user-content-derived facts (URLs, file
-- paths, shell commands). LLM outputs are large, low-cardinality,
-- AI-derived narratives — different lifecycle (user may delete one,
-- want to retain forever, export separately) and different access
-- pattern (fetched by session_id or by kind, not searched by value).

CREATE TABLE llm_outputs (
    id             INTEGER PRIMARY KEY,

    -- session_id is the session this output is ABOUT. NULL for
    -- multi-session outputs (reflect/propose may summarize a week
    -- of work). ON DELETE SET NULL so deleting a session preserves
    -- its summary as historical record, just detached.
    session_id     TEXT REFERENCES sessions(id) ON DELETE SET NULL,

    -- kind ∈ {summary, reflect, propose}. Enforced in application
    -- code; keeping the schema a free TEXT lets Block D+ add new
    -- output kinds without a migration.
    kind           TEXT NOT NULL,

    -- model is the provider's reported identifier (e.g.
    -- claude-sonnet-4-6). Lets us reason about "which model
    -- produced this" when comparing outputs over time.
    model          TEXT NOT NULL,

    -- prompt_hash is a sha256 hex digest of the full prompt (system
    -- + messages) the caller built. The UNIQUE index on
    -- (kind, prompt_hash) is what lets `summarize --force=false`
    -- return the cached output rather than re-paying for tokens.
    prompt_hash    TEXT NOT NULL,

    -- Token accounting the provider reported back. Nullable: not all
    -- providers return usage data.
    input_tokens   INTEGER,
    output_tokens  INTEGER,

    -- body is the assembled text reply. Large but bounded by the
    -- model's max_tokens setting (typically < 64 KB).
    body           TEXT NOT NULL,

    -- created_at_ms is client-clock at the moment we persisted.
    created_at_ms  INTEGER NOT NULL
);

-- Lookup by (kind, prompt_hash): the summarize/reflect/propose
-- commands use this as a cache check before calling the LLM.
CREATE UNIQUE INDEX idx_llm_outputs_hash ON llm_outputs(kind, prompt_hash);

-- Listing outputs for one session (summarize --session X history).
-- Partial index skips the multi-session rows that carry NULL.
CREATE INDEX idx_llm_outputs_session_kind
    ON llm_outputs(session_id, kind)
    WHERE session_id IS NOT NULL;

-- Listing recent outputs regardless of session (reflect dashboards).
CREATE INDEX idx_llm_outputs_created ON llm_outputs(created_at_ms DESC);

UPDATE meta SET value='2' WHERE key='schema_version';
```

## `003_sessions_effective_ts_index.sql`

```sql
-- Expression index on the "effective" session timestamp used by the
-- time-window queries in events.go (LoadRecentSessionDigests) and the
-- MCP list_sessions tool. Both filter and ORDER BY the same COALESCE
-- expression; without an index on it SQLite scans sessions every time.
-- Acceptable at tens of sessions; starts to hurt past a few thousand.

CREATE INDEX idx_sessions_effective_ts
    ON sessions(COALESCE(ended_at_ms, started_at_ms, 0) DESC);

UPDATE meta SET value='3' WHERE key='schema_version';
```
