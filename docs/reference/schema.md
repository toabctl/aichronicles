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

## `004_search_tokenizer.sql`

```sql
-- Search tokenizer overhaul.
--
-- Migration 001 set up events_fts with tokenize='porter unicode61'.
-- Two problems with that choice:
--
--   * Porter stems English words (`migrate` -> `migrat`, `running` ->
--     `run`). Helpful for prose; actively wrong for code identifiers
--     and file paths, where exact spellings carry meaning.
--
--   * unicode61 alone treats `_`, `-`, `.`, `/` as token characters,
--     so `internal/store/migrate.go` is one giant token nobody can
--     match. Searches for `migrate` or `store` came back empty
--     unless the user happened to type the whole path.
--
-- This migration drops events_fts and recreates it with:
--
--   tokenize="unicode61 separators '_-./'"
--
-- which leaves unicode word/digit handling intact but forces the
-- four code-friendly characters to act as token separators. Porter
-- is gone — internal/searchquery appends `*` to bare tokens so
-- `shutdown` still finds `shutdowns` via prefix match without paying
-- the stemmer's accuracy cost on identifiers.
--
-- FTS5 doesn't support changing the tokenizer in place. We drop the
-- triggers first (they reference events_fts), drop the table, then
-- recreate everything and backfill from events. The whole migration
-- runs in a single transaction (see internal/store/migrate.go), so
-- a partial failure rolls back to schema_version=3 cleanly.

DROP TRIGGER IF EXISTS events_fts_ai;
DROP TRIGGER IF EXISTS events_fts_ad;
DROP TRIGGER IF EXISTS events_fts_au;
DROP TABLE IF EXISTS events_fts;

CREATE VIRTUAL TABLE events_fts USING fts5(
    content_text,
    content='events',
    content_rowid='rowid',
    tokenize="unicode61 separators '_-./'"
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

-- Backfill the new index from the events table. O(n) one-time cost;
-- a personal-use store is in the tens of thousands of rows at most,
-- well under a second.
INSERT INTO events_fts(rowid, content_text)
    SELECT rowid, content_text FROM events;

UPDATE meta SET value='4' WHERE key='schema_version';
```

## `005_search_trigram.sql`

```sql
-- Trigram fallback index for substring matches.
--
-- Migration 004 fixed the primary FTS5 tokenizer so identifiers and
-- paths split on `_-./`, which solves "find the session that touched
-- migrate.go." It does NOT solve "find the session about MongoDB
-- when I only remember typing `MongoD`": MongoD is a prefix of one
-- token (MongoDB) and the searchquery layer correctly turns it into
-- `MongoD*`, but only when the tokenizer leaves MongoDB intact as one
-- word. If the corpus has MongoDb spelled MongoDB, the prefix works;
-- if a different spelling exists or the user remembers a substring
-- from the middle, the primary index is no help.
--
-- The trigram tokenizer (built into SQLite ≥ 3.34) splits content
-- into 3-character sliding windows, so MATCH 'mongoD*' against the
-- trigram index hits any doc containing the trigrams `mon`, `ong`,
-- `ngo`, `goD`, ... in sequence — which is what people mean by
-- "substring search."
--
-- This index lives alongside events_fts; SearchEvents tries the
-- primary first and falls back to trigram only when the primary
-- returns zero hits. That keeps the common case (whole-word
-- matches) on the smaller, faster word index, and pays the trigram
-- cost only when it actually buys something.

CREATE VIRTUAL TABLE events_fts_trigram USING fts5(
    content_text,
    content='events',
    content_rowid='rowid',
    tokenize='trigram'
);

CREATE TRIGGER events_fts_trigram_ai AFTER INSERT ON events BEGIN
    INSERT INTO events_fts_trigram(rowid, content_text) VALUES (new.rowid, new.content_text);
END;
CREATE TRIGGER events_fts_trigram_ad AFTER DELETE ON events BEGIN
    INSERT INTO events_fts_trigram(events_fts_trigram, rowid, content_text) VALUES ('delete', old.rowid, old.content_text);
END;
CREATE TRIGGER events_fts_trigram_au AFTER UPDATE ON events BEGIN
    INSERT INTO events_fts_trigram(events_fts_trigram, rowid, content_text) VALUES ('delete', old.rowid, old.content_text);
    INSERT INTO events_fts_trigram(rowid, content_text) VALUES (new.rowid, new.content_text);
END;

INSERT INTO events_fts_trigram(rowid, content_text)
    SELECT rowid, content_text FROM events;

UPDATE meta SET value='5' WHERE key='schema_version';
```

## `006_extractions_fts.sql`

```sql
-- Extractions FTS index — typed-fact fallback for search.
--
-- The extractions table already stores three classes of structured
-- facts pulled out of envelopes by pkg/ingest/extract:
--
--   * url             — http(s) URLs the agent referenced
--   * file_path       — files touched via Read/Write/Edit/NotebookEdit
--   * shell_command   — Bash invocations
--
-- These often don't appear verbatim in events.content_text. A user
-- who opened internal/store/migrate.go via Read may have no message
-- mentioning the path; the fact lives only in extractions. Today,
-- searching for `migrate.go` misses such sessions even though they
-- demonstrably touched the file.
--
-- This migration adds events_fts_trigram's sibling for the typed
-- table: a contentless FTS5 index over extractions.value, with the
-- same code-friendly tokenizer (unicode61 with `_-./` separators) so
-- paths, URLs, and command lines split into useful tokens. Search
-- code will consult this index as a third-tier fallback after
-- events_fts and events_fts_trigram both miss; events synthesised
-- from the matching extractions get a snippet labelled with the
-- extraction kind so callers know the hit came via a typed fact.

CREATE VIRTUAL TABLE extractions_fts USING fts5(
    value,
    content='extractions',
    content_rowid='rowid',
    tokenize="unicode61 separators '_-./'"
);

CREATE TRIGGER extractions_fts_ai AFTER INSERT ON extractions BEGIN
    INSERT INTO extractions_fts(rowid, value) VALUES (new.rowid, new.value);
END;
CREATE TRIGGER extractions_fts_ad AFTER DELETE ON extractions BEGIN
    INSERT INTO extractions_fts(extractions_fts, rowid, value) VALUES ('delete', old.rowid, old.value);
END;
CREATE TRIGGER extractions_fts_au AFTER UPDATE ON extractions BEGIN
    INSERT INTO extractions_fts(extractions_fts, rowid, value) VALUES ('delete', old.rowid, old.value);
    INSERT INTO extractions_fts(rowid, value) VALUES (new.rowid, new.value);
END;

INSERT INTO extractions_fts(rowid, value)
    SELECT rowid, value FROM extractions;

UPDATE meta SET value='6' WHERE key='schema_version';
```

## `007_subagent_threads.sql`

```sql
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
```

## `008_ingest_seq_table.sql`

```sql
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
```

## `009_event_embeddings.sql`

```sql
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
```
