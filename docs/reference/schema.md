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

## `010_session_links.sql`

```sql
-- 010_session_links.sql
--
-- Adds a session_links table for cross-session link induction.
-- Inspired by A-Mem (arXiv:2502.12110) — a Zettelkasten-style
-- memory system where each note links to related prior notes,
-- letting the agent retrieve "the time we already solved this"
-- instead of starting from scratch every session.
--
-- aichronicles' analog: at summarize time the LLM is shown a
-- shortlist of candidate prior sessions (same cwd, recent) and
-- asked to emit typed links — builds_on, repeats_failure_of,
-- supersedes, related — for ones that genuinely connect to the
-- session being summarized. Links are persisted here and surfaced
-- on /sessions/<id> as a "Related sessions" sidebar.
--
-- Design notes:
--
--   - (from, to, kind) is the primary key — a session can connect
--     to the same prior session via different kinds (e.g. both
--     `builds_on` and `related`) but never twice via the same
--     kind. Re-running summarize on the same session replaces.
--
--   - kind is a TEXT with a CHECK so the set is closed: typos in
--     a future LLM call won't silently land a fifth category.
--
--   - rationale is one short LLM-emitted line ("repeats the same
--     auth-middleware fix from session abc12345"). Optional, but
--     usually present and worth surfacing in the UI.
--
--   - ON DELETE CASCADE on both sides — when a session is purged
--     the links go with it, no dangling rows.
CREATE TABLE session_links (
    from_session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    to_session_id   TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL CHECK (kind IN ('builds_on', 'repeats_failure_of', 'supersedes', 'related')),
    rationale       TEXT,
    created_at_ms   INTEGER NOT NULL,
    PRIMARY KEY (from_session_id, to_session_id, kind)
);

-- Reverse-lookup index: "show me everything that links TO session
-- X" powers the incoming-links half of the sidebar. The forward
-- direction is already covered by the PK.
CREATE INDEX idx_session_links_to ON session_links(to_session_id);

INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', '10');
```

## `011_drop_unused_indexes.sql`

```sql
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
```

## `012_events_transport.sql`

```sql
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
```

## `013_drop_tool_call_index.sql`

```sql
-- Drop idx_events_tool_call.
--
-- Migration 011 left this index in place under the assumption that
-- a future tool_use ↔ tool_result pairing feature would need it.
-- The user has confirmed the feature isn't on the near-term roadmap,
-- and write amplification on every event ingest is paid for nothing
-- in the meantime. Easy to add back when the feature lands.

DROP INDEX IF EXISTS idx_events_tool_call;

UPDATE meta SET value='13' WHERE key='schema_version';
```

## `014_events_provenance.sql`

```sql
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
```

## `015_sessions_start_cwd.sql`

```sql
-- Add sessions.start_cwd as a first-class column.
--
-- sessions.cwd is maintained by the AFTER-INSERT trigger from
-- migration 001 to track the LATEST non-null cwd seen — useful when
-- the user cd's mid-session and we want to know "where did this
-- session end up?" But several callers actually want the START
-- cwd: the directory the session was launched in. They reconstruct
-- it via expensive correlated subqueries:
--
--   * LoadSessionStartCwd: SELECT MIN(ts_source_ms) WHERE cwd NOT NULL
--   * LoadProjectAggregates: a 9-line CTE re-deriving the same value
--
-- Both are slow at scale and brittle. Materializing start_cwd as a
-- column collapses both query paths to a column read. The existing
-- sessions.cwd column keeps its "last non-null" semantics — callers
-- that genuinely want that (the resume button when the user cd'd
-- away and back) keep working unchanged.
--
-- Nullable on purpose: a session with zero events has no cwd to
-- carry. Callers fall back to s.cwd or hide the dependent UI when
-- start_cwd IS NULL.

ALTER TABLE sessions ADD COLUMN start_cwd TEXT;

-- Backfill: first non-null cwd per session in event-time order.
-- Tie-break on rowid (matching the LoadSessionStartCwd query)
-- so two events with the same ts_source_ms resolve deterministically.
UPDATE sessions
   SET start_cwd = (
       SELECT cwd FROM events
        WHERE session_id = sessions.id AND cwd IS NOT NULL
        ORDER BY ts_source_ms ASC, rowid ASC
        LIMIT 1
   );

-- Trigger to keep start_cwd current. Fires only on the FIRST
-- non-null cwd: subsequent cwd changes (the user cd'd into a
-- subdir mid-session) leave start_cwd alone. This is the property
-- the existing sessions_agg_ai trigger does NOT provide — that one
-- always overwrites cwd with the latest seen.
CREATE TRIGGER sessions_start_cwd_ai
AFTER INSERT ON events
WHEN new.cwd IS NOT NULL
BEGIN
    UPDATE sessions
       SET start_cwd = new.cwd
     WHERE id = new.session_id AND start_cwd IS NULL;
END;

UPDATE meta SET value='15' WHERE key='schema_version';
```

## `016_sessions_first_prompt_summary_topic.sql`

```sql
-- Materialize sessions.first_prompt_text and sessions.summary_topic.
--
-- Eight callers across cli/mcp/web/store re-derive "the first
-- user_prompt content_text for this session" via the same
-- correlated subquery; four more re-derive "the topic field from
-- the latest summary body" via a json_extract subquery. Every
-- session-list render pays the cost. The audit identified these
-- as the highest-volume duplications.
--
-- Materialising both as columns on sessions, kept current by
-- AFTER INSERT triggers, collapses the per-row subqueries to a
-- column read. The audit's task #24 (summary topic JSON parse)
-- and task #21 (the unified session-list reader) both become
-- trivial against this shape — callers either read the column or
-- they don't, no more parametric subquery composition.
--
-- Both columns are nullable: a session with no user_prompt yet,
-- or no cached summary yet, has nothing to project. Callers
-- fall back to "(no prompt)" / "(no summary)" the same way they
-- did before.
--
-- Triggers:
--
--   sessions_first_prompt_ai: fires on every event INSERT, updates
--     first_prompt_text only when it's currently NULL and the new
--     event is a user_prompt with non-null content. Sticky on the
--     first user_prompt seen.
--
--   sessions_summary_topic_ai: fires on every llm_outputs INSERT
--     where kind='summary', overwrites summary_topic from
--     json_extract(body, '$.topic'). Always reflects the LATEST
--     summary so the rolling re-summarize-after-edit case works.

ALTER TABLE sessions ADD COLUMN first_prompt_text TEXT;
ALTER TABLE sessions ADD COLUMN summary_topic     TEXT;

-- Backfill first_prompt_text from the existing correlated subquery
-- shape (oldest user_prompt with non-null content_text per session).
UPDATE sessions
   SET first_prompt_text = (
       SELECT content_text FROM events
        WHERE session_id = sessions.id
          AND kind = 'user_prompt'
          AND content_text IS NOT NULL
        ORDER BY ts_source_ms ASC, rowid ASC
        LIMIT 1
   );

-- Backfill summary_topic from the latest kind=summary llm_outputs
-- per session, parsed via SQLite's built-in json_extract. Guarded
-- with json_valid so a malformed body (legacy prose, partial write)
-- doesn't crash the migration — it just leaves summary_topic NULL
-- for that session, which callers already handle.
UPDATE sessions
   SET summary_topic = (
       SELECT CASE WHEN json_valid(body)
                   THEN json_extract(body, '$.topic')
                   ELSE NULL END
         FROM llm_outputs
        WHERE session_id = sessions.id AND kind = 'summary'
        ORDER BY created_at_ms DESC
        LIMIT 1
   );

-- First-prompt trigger: re-derive from the canonical
-- "earliest-timestamp user_prompt with non-null content" rule on
-- every user_prompt insert. This pays a one-row indexed lookup per
-- write but matches the previous correlated-subquery semantics
-- exactly — including the edge case where imports backfill an
-- older user_prompt after a newer one already landed via the live
-- hook. A simpler "set when NULL" trigger would silently lose
-- that case (the new earlier prompt arrives, but first_prompt_text
-- stays anchored to the later live one). The full-rederive cost
-- is paid on writes (cheap, indexed) so reads stay free.
CREATE TRIGGER sessions_first_prompt_ai
AFTER INSERT ON events
WHEN new.kind = 'user_prompt' AND new.content_text IS NOT NULL
BEGIN
    UPDATE sessions
       SET first_prompt_text = (
           SELECT e.content_text FROM events e
            WHERE e.session_id = new.session_id
              AND e.kind = 'user_prompt'
              AND e.content_text IS NOT NULL
            ORDER BY e.ts_source_ms ASC, e.rowid ASC
            LIMIT 1
       )
     WHERE id = new.session_id;
END;

-- Latest-wins summary-topic trigger. Guarded by json_valid so a
-- malformed body (the test fixtures use plain strings; legacy
-- prose summaries from before record_summary tool calls landed)
-- doesn't break the INSERT. Bodies that don't parse leave
-- summary_topic NULL — same fallback callers handle today via
-- extractSummaryTopic returning empty string.
CREATE TRIGGER sessions_summary_topic_ai
AFTER INSERT ON llm_outputs
WHEN new.kind = 'summary' AND new.session_id IS NOT NULL
BEGIN
    UPDATE sessions
       SET summary_topic = CASE
           WHEN json_valid(new.body)
           THEN json_extract(new.body, '$.topic')
           ELSE NULL
       END
     WHERE id = new.session_id;
END;

UPDATE meta SET value='16' WHERE key='schema_version';
```

## `017_session_outcomes.sql`

```sql
-- Materialise per-session outcome signals.
--
-- aichronicles is a read-only-traces system: it observes Claude Code /
-- Gemini CLI sessions but cannot re-roll them, has no env oracle, and
-- gets no binary reward per task. The literature's smallest viable
-- feedback signal for retrieval-based self-improvement is binary task
-- success (Self-Generated In-Context Examples shows even naive
-- accumulation gives +10–15 absolute points if you can filter for it).
--
-- Without an oracle we settle for proxies — observable patterns in the
-- raw event stream that correlate with "this session did/did not go
-- well." The signals here are intentionally conservative (CLAUDE.md
-- rule 7: correctness over coverage). Each is computed from immutable
-- raw_envelopes / events / extractions, so any row can be deleted and
-- recomputed without information loss.
--
-- Signals captured (raw counts, no thresholds):
--   * user_prompt_count, tool_use_count, tool_failure_count,
--     error_count, compact_count — direct kind aggregates over events
--   * git_undo_count — shell_command extractions matching a narrow set
--     of unambiguous-undo patterns (git reset --hard, git revert,
--     git checkout -- <path>, git restore, git stash push/save). Plain
--     `git reset HEAD` (just unstaging) is deliberately excluded.
--   * prompt_repeat_count — count of consecutive user_prompts whose
--     content_text is byte-for-byte equal after lowercase + whitespace
--     normalisation. False negatives but no false positives.
--   * last_event_kind — the kind of the chronologically last event in
--     the session, useful for "ended on tool_failure" detection
--     without requiring callers to re-query.
--
-- A coarse `outcome` label is derived from the above using rules
-- documented in ComputeSessionOutcome (Go side). Values:
--   * success_likely — clean activity, no failure markers
--   * failure_likely — strong failure signals (failures, undo, repeat)
--   * mixed          — real activity with weak failure signals
--   * unknown        — too thin to label (no tool use, no progress)
--
-- The `_likely` suffix is deliberate: these are heuristics over
-- observational data, not ground truth. Downstream consumers
-- (propose, induction, reflect) read them as priors, not facts.
--
-- Computation is lazy: rows are written by ComputeSessionOutcome /
-- SaveSessionOutcome called by RunPropose and RunInductionForSession
-- before they build their digests. There is no AFTER-INSERT trigger:
-- prompt-repeat detection requires walking user_prompt content_text
-- in chronological order, and git-undo detection requires extraction
-- joins — both too involved for a SQLite trigger. The raw_envelopes
-- invariant (sacred, append-only) means recomputation is always safe.

CREATE TABLE session_outcomes (
    session_id          TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    computed_at_ms      INTEGER NOT NULL,

    -- Raw kind counts.
    user_prompt_count   INTEGER NOT NULL DEFAULT 0,
    tool_use_count      INTEGER NOT NULL DEFAULT 0,
    tool_failure_count  INTEGER NOT NULL DEFAULT 0,
    error_count         INTEGER NOT NULL DEFAULT 0,
    compact_count       INTEGER NOT NULL DEFAULT 0,

    -- Derived signals (require Go-level scan).
    git_undo_count      INTEGER NOT NULL DEFAULT 0,
    prompt_repeat_count INTEGER NOT NULL DEFAULT 0,

    -- Optional shape hint.
    last_event_kind     TEXT,

    -- Coarse derived label (rules in ComputeSessionOutcome).
    outcome             TEXT NOT NULL CHECK(outcome IN
        ('success_likely', 'failure_likely', 'mixed', 'unknown'))
);

-- Index for "find recent failures" / "find recent successes" reads
-- without scanning the whole table. Computed_at_ms is the natural
-- secondary sort for "show me the latest 50 failure_likely sessions".
CREATE INDEX idx_session_outcomes_outcome ON session_outcomes(outcome, computed_at_ms DESC);

UPDATE meta SET value='17' WHERE key='schema_version';
```

## `018_proposed_skills.sql`

```sql
-- Track the lifecycle of every skill the LLM proposes.
--
-- Until this migration, the propose → apply flow is open-loop: the
-- LLM emits N skill proposals, the user runs `propose apply --skill
-- <name>` for the ones they like, the SKILL.md lands on disk, and
-- nothing further is recorded. Down-stream propose runs see "skills
-- installed" and "skills invoked" but not the provenance — was a
-- skill on disk authored by hand or scaffolded from a prior
-- proposal? Was a proposed skill ever applied at all? Did applied
-- proposals get used afterward?
--
-- That gap is the load-bearing missing piece in the AWM (Agent
-- Workflow Memory) loop: without proposal lifecycle data the system
-- cannot tell its own past suggestions from random skill files, and
-- cannot evaluate whether its proposals actually helped.
--
-- This table is the index. Every (llm_output_id, skill_name) pair
-- the propose / induction / skill-revision paths emit gets one row
-- on persistence. apply updates applied_at_ms + applied_path. A
-- later proposal with the same skill_name marks the prior row
-- superseded_by_id (newest wins for "which proposal birthed the
-- skill on disk today"). Each transition is monotonic in
-- proposed_at_ms so the historical sequence stays reconstructable.
--
-- proposed_at_ms is NOT NULL — every row was proposed at some
-- moment. applied_at_ms IS NULL when the proposal was never
-- accepted; that's the abandonment-rate signal — proposals the LLM
-- thought were worthwhile but the user did not.
--
-- llm_output_id is the FK to the propose / induction llm_outputs
-- row. ON DELETE CASCADE so cleanup of cache entries flows through.
-- skill_name is kebab-case canonical (matches the proposal schema's
-- ^[a-z][a-z0-9-]*$ pattern). UNIQUE(llm_output_id, skill_name) is
-- the natural key — one proposal output may name a skill at most
-- once.

CREATE TABLE proposed_skills (
    id                INTEGER PRIMARY KEY,
    llm_output_id     INTEGER NOT NULL REFERENCES llm_outputs(id) ON DELETE CASCADE,
    skill_name        TEXT NOT NULL,
    proposed_at_ms    INTEGER NOT NULL,
    applied_at_ms     INTEGER,
    applied_path      TEXT,
    superseded_by_id  INTEGER REFERENCES proposed_skills(id) ON DELETE SET NULL,
    UNIQUE(llm_output_id, skill_name)
);

-- "Find every proposal of skill X across history" + lookup-by-name
-- joins from extractions(skill_load).value.
CREATE INDEX idx_proposed_skills_name ON proposed_skills(skill_name);

-- Partial index for "show me everything that's actually on disk."
-- The applied subset is small (~tens of rows) — full-table scans
-- on the unfiltered column would still be fast at our scale, but
-- partial keeps the query plan obviously right under EXPLAIN.
CREATE INDEX idx_proposed_skills_applied
    ON proposed_skills(applied_at_ms DESC)
    WHERE applied_at_ms IS NOT NULL;

UPDATE meta SET value='18' WHERE key='schema_version';
```

## `019_semantic_facts.sql`

```sql
-- Materialise the SEMANTIC memory type — typed facts derived from
-- sessions, distinct from episodic memory (events) and procedural
-- memory (skills + workflows + propose).
--
-- Today's flat memory model funnels everything through events →
-- extractions → llm_outputs. The literature has converged on TYPED
-- memory (MIRIX taxonomy: core / episodic / semantic / procedural /
-- resource / knowledge-vault) where retrieval is gated by type:
-- procedural at task-planning time, semantic at fact-grounding
-- time, episodic only when explicitly searching. The win is that
-- "what do I know about this project's build system?" doesn't have
-- to scan unrelated event content.
--
-- This migration adds the SEMANTIC layer scoped to cwd-anchored
-- facts ("project at /work/aichronicles uses Go 1.26", "test
-- command is go test ./...", "deploys via systemd timer"). Other
-- subjects (broader concepts, person-scoped facts) are out of
-- scope for v1: cwd-anchored is the highest-leverage shape because
-- it's the natural retrieval key when the next session opens in
-- the same project.
--
-- One row per (subject, predicate, object) triple via the natural-
-- key UNIQUE. Re-asserting the same triple updates confidence /
-- asserted_at_ms / evidence pointer (the latest evidence wins);
-- conflicting objects (e.g. "go 1.25" and "go 1.26" for the same
-- subject+predicate) coexist as separate rows so the truth is
-- never silently overwritten — callers pick by asserted_at_ms.
--
-- source_llm_output_id ties each fact back to the LLM run that
-- emitted it. ON DELETE CASCADE so a cache eviction flows through;
-- evidence_session_id ON DELETE SET NULL preserves the fact even
-- if its grounding session gets pruned (the fact is still claimed
-- by the LLM_output, just no longer drillable).
--
-- Predicates are intentionally NOT CHECK-constrained: the
-- recommended set is documented in store/facts.go but free-form is
-- accepted. A controlled vocabulary at the SQL level would force a
-- new migration each time a useful predicate emerges; the LLM
-- prompt does the gating instead.

CREATE TABLE semantic_facts (
    id                    INTEGER PRIMARY KEY,
    source_llm_output_id  INTEGER NOT NULL REFERENCES llm_outputs(id) ON DELETE CASCADE,
    subject               TEXT NOT NULL,
    predicate             TEXT NOT NULL,
    object                TEXT NOT NULL,
    confidence            REAL NOT NULL DEFAULT 1.0
                          CHECK(confidence >= 0.0 AND confidence <= 1.0),
    evidence_session_id   TEXT REFERENCES sessions(id) ON DELETE SET NULL,
    evidence_quote        TEXT,
    asserted_at_ms        INTEGER NOT NULL,
    UNIQUE(subject, predicate, object)
);

-- Subject is the dominant access key — "what do I know about this
-- cwd?" is a hot query. Composite (subject, predicate) so
-- predicate-narrowed reads ("what test command for this project?")
-- still hit the index leftmost.
CREATE INDEX idx_semantic_facts_subject ON semantic_facts(subject, predicate);

-- "Show me the most recently asserted facts" for the CLI list
-- command and MCP introspection. Partial index on non-NULL
-- evidence to skip facts whose grounding has been pruned (rare).
CREATE INDEX idx_semantic_facts_recent ON semantic_facts(asserted_at_ms DESC);

UPDATE meta SET value='19' WHERE key='schema_version';
```

## `020_drop_event_embeddings.sql`

```sql
-- Round 12: drop the embedding system entirely.
--
-- event_embeddings was added in migration 009 to back semantic
-- search (CLI / MCP), workflow task_shape ranking, and the
-- summarize candidate-prior rerank pass. After real-use review
-- the embedding layer was an optional enhancement, not a
-- foundation:
--
--   * the core self-improvement loop (summarize → induction →
--     facts) never read or wrote embeddings;
--   * SQLite FTS5 with porter stemming handled the bulk of
--     keyword retrieval needs honestly well at personal-use
--     scale;
--   * the candidate-prior rerank had a recency-only fallback
--     that was the correct pre-Round-3 default;
--   * embeddings cost an extra provider dependency (OpenAI,
--     since Anthropic doesn't expose hosted embeddings).
--
-- Dropping the table reclaims storage and removes a maintenance
-- surface (provider adapter, embedder worker, three retrieval
-- handlers). The data is irrecoverable; the user explicitly
-- opted into "ignore backwards compat" before this round.
--
-- event_embeddings carried an FK INTO events.event_id but no
-- other table referenced it, so the drop is clean — no cascade
-- fallout, no orphan rows.

DROP TABLE IF EXISTS event_embeddings;

UPDATE meta SET value='20' WHERE key='schema_version';
```

## `021_skill_candidates.sql`

```sql
-- Migration 021: adopt the AutoSkill (Yang et al., 2026 — arXiv:2603.01145)
-- vocabulary and lifecycle for the candidate-skill table.
--
-- AutoSkill formalises the field's standard skill-induction loop as
--
--   experience ingestion → skill extraction → skill maintenance → skill reuse
--
-- where each newly-extracted candidate is followed by exactly one
-- maintenance decision a_t ∈ {add, merge, discard}. Pre-migration
-- aichronicles tracked only an implicit two-state version of this
-- lifecycle (applied_at_ms set ↔ "added"; NULL ↔ "pending"), which
-- conflated "the user has not decided yet" with "the user actively
-- declined" and gave no place to record a merge target. This
-- migration aligns the schema with the standard vocabulary so the
-- merge-not-replace path can be wired up cleanly.
--
-- Renames:
--   proposed_skills      → skill_candidates       (table)
--   applied_at_ms        → decision_at_ms         (now generic across add/merge/discard)
--   applied_path         → add_path               (only meaningful for decision='add')
--
-- New columns:
--   decision        TEXT      'add' | 'merge' | 'discard' | NULL=pending
--   merged_into_id  INTEGER   FK to skill_candidates(id), set when decision='merge'
--   triggers        TEXT      JSON array of strings — AutoSkill τ
--   tags            TEXT      JSON array of strings — AutoSkill γ
--   examples        TEXT      JSON array of {input, output} — AutoSkill ξ
--   version         TEXT      semver-ish ("v0.1.0") — AutoSkill v; merge bumps the patch
--
-- Backfill: any pre-migration row with decision_at_ms (= the old
-- applied_at_ms) set was the previous code's "applied" path, which
-- corresponds to AutoSkill action 'add'. Pending rows stay NULL.
--
-- Indexes are dropped+recreated under the new table name. SQLite
-- silently rewrites the partial index predicate to use the renamed
-- column, but the index identifier stays as-is, which makes
-- .schema confusing — the rebuild keeps the schema readable.
--
-- The pre-existing superseded_by_id column (declared in migration
-- 018 but never written by any code path, and a dead variant of the
-- merge-target idea this migration introduces cleanly) cannot be
-- dropped in place because SQLite forbids DROP COLUMN on a column
-- referenced by a self-FK constraint. Leaving it as dead schema
-- here; a follow-up table-recreate migration can remove it once
-- it's worth the churn.

ALTER TABLE proposed_skills RENAME TO skill_candidates;

ALTER TABLE skill_candidates RENAME COLUMN applied_at_ms TO decision_at_ms;
ALTER TABLE skill_candidates RENAME COLUMN applied_path  TO add_path;

ALTER TABLE skill_candidates ADD COLUMN decision       TEXT;
ALTER TABLE skill_candidates ADD COLUMN merged_into_id INTEGER
    REFERENCES skill_candidates(id) ON DELETE SET NULL;
ALTER TABLE skill_candidates ADD COLUMN triggers TEXT;
ALTER TABLE skill_candidates ADD COLUMN tags     TEXT;
ALTER TABLE skill_candidates ADD COLUMN examples TEXT;
ALTER TABLE skill_candidates ADD COLUMN version  TEXT;

UPDATE skill_candidates
   SET decision = 'add'
 WHERE decision_at_ms IS NOT NULL;

DROP INDEX IF EXISTS idx_proposed_skills_name;
DROP INDEX IF EXISTS idx_proposed_skills_applied;

CREATE INDEX idx_skill_candidates_name ON skill_candidates(skill_name);
CREATE INDEX idx_skill_candidates_decided
    ON skill_candidates(decision_at_ms DESC)
    WHERE decision_at_ms IS NOT NULL;

UPDATE meta SET value='21' WHERE key='schema_version';
```

## `022_drop_superseded_by_id.sql`

```sql
-- Migration 022: drop the unused superseded_by_id column.
--
-- superseded_by_id was declared in migration 018 ("a later proposal
-- with the same skill_name marks the prior row superseded_by_id")
-- but no code path ever wrote to it. Migration 021 introduced the
-- AutoSkill (Yang et al., 2026 — arXiv:2603.01145) lifecycle with
-- merged_into_id playing the actual "this row was replaced by a
-- later one" role; superseded_by_id has been a dead column ever
-- since. Migration 021 left it in place because SQLite's DROP
-- COLUMN refuses to drop a column referenced by its own foreign
-- key constraint (superseded_by_id has REFERENCES skill_candidates
-- (id)) and table-recreates require care around self-FKs.
--
-- This migration takes that care: defer_foreign_keys=ON inside the
-- migration transaction so the merged_into_id self-FK on the new
-- table doesn't fire mid-INSERT, while the existing data is copied
-- across with IDs preserved.
--
-- The migration runner wraps this whole script in a transaction
-- (see store.applyMigration); defer_foreign_keys auto-resets on
-- commit per SQLite docs, so callers see the normal foreign_keys=ON
-- semantics afterwards.

PRAGMA defer_foreign_keys = ON;

CREATE TABLE skill_candidates_new (
    id              INTEGER PRIMARY KEY,
    llm_output_id   INTEGER NOT NULL REFERENCES llm_outputs(id) ON DELETE CASCADE,
    skill_name      TEXT NOT NULL,
    proposed_at_ms  INTEGER NOT NULL,
    decision_at_ms  INTEGER,
    add_path        TEXT,
    decision        TEXT,
    merged_into_id  INTEGER REFERENCES skill_candidates_new(id) ON DELETE SET NULL,
    triggers        TEXT,
    tags            TEXT,
    examples        TEXT,
    version         TEXT,
    UNIQUE(llm_output_id, skill_name)
);

INSERT INTO skill_candidates_new
       (id, llm_output_id, skill_name, proposed_at_ms,
        decision_at_ms, add_path, decision, merged_into_id,
        triggers, tags, examples, version)
SELECT  id, llm_output_id, skill_name, proposed_at_ms,
        decision_at_ms, add_path, decision, merged_into_id,
        triggers, tags, examples, version
  FROM skill_candidates;

DROP TABLE skill_candidates;

ALTER TABLE skill_candidates_new RENAME TO skill_candidates;

-- Indexes: the originals from migration 021 lived on the dropped
-- table; recreate them on the new one. CREATE INDEX is idempotent
-- via IF NOT EXISTS so a partially-applied state would still
-- converge.
CREATE INDEX IF NOT EXISTS idx_skill_candidates_name
    ON skill_candidates(skill_name);
CREATE INDEX IF NOT EXISTS idx_skill_candidates_decided
    ON skill_candidates(decision_at_ms DESC)
    WHERE decision_at_ms IS NOT NULL;

UPDATE meta SET value='22' WHERE key='schema_version';
```

## `023_skill_candidates_provenance.sql`

```sql
-- Migration 023: tamper-evidence for added skills (SSGM provenance hash).
--
-- SSGM (Lam et al., 2026 — arXiv:2603.11768) prescribes that
-- governance for an evolving memory bank requires provenance and
-- integrity primitives, not just retrieval scoring. The lightest-
-- weight version aichronicles can ship is a content hash of the
-- rendered SKILL.md body, captured at write time and stored on the
-- skill_candidates row alongside add_path.
--
-- Drift detection becomes possible: a later sweep reads the
-- on-disk SKILL.md, hashes its body, and compares against
-- add_body_sha256 — a mismatch flags either a hand-edit, an
-- external rewrite, or filesystem corruption. Without the column
-- the lifecycle index has no way to tell "this skill is what we
-- wrote" from "this skill is what someone else wrote afterwards."
--
-- NULL when decision != 'add' (no on-disk file to hash) or when
-- the skill was added before this migration.

ALTER TABLE skill_candidates ADD COLUMN add_body_sha256 TEXT;

UPDATE meta SET value='23' WHERE key='schema_version';
```

## `024_skill_candidates_kind.sql`

```sql
-- Migration 024: skill_candidates.kind — pattern vs pitfall.
--
-- Until now every induced / proposed skill encoded the same shape:
-- "when condition X fires, do procedure Y" — the success-pattern
-- form. EvoSkill (Sentient/VT, 2026 — arXiv:2603.02766) and EvoSC
-- (2602.01966) both argue that failure-driven induction is the
-- complementary half: extract a "when X is about to fail, AVOID
-- doing Y" rule from sessions where things went wrong, and stash
-- that as a distinct skill kind.
--
-- aichronicles already has the data: session_outcomes flags
-- failure_likely sessions, and the propose system prompt rule 13
-- already invites the LLM to consider prevention skills. What was
-- missing was a label on the candidate so we can DISTINGUISH a
-- pattern (do-this) skill from a pitfall (avoid-this) skill across
-- the lifecycle — the metrics, the merge gate, the SKILL.md
-- frontmatter, and the eventual evolve / discard sweeps all want
-- to treat the two differently.
--
-- Default 'pattern' covers every legacy row: the existing
-- candidates were extracted from successful or mixed sessions
-- under the success-pattern frame, and labelling them retro-
-- actively as 'pattern' is faithful to what the LLM emitted.

ALTER TABLE skill_candidates ADD COLUMN kind TEXT NOT NULL DEFAULT 'pattern';

UPDATE meta SET value='24' WHERE key='schema_version';
```

## `025_episodes.sql`

```sql
-- Migration 025: episodes — instance-specific contextually-bound
-- retrievable units of session experience.
--
-- Pink et al. (2026 — arXiv:2502.06975, "Episodic Memory is the
-- Missing Piece for Long-Term LLM Agents") argue that the field's
-- universal gap is the *episodic* memory slot: a chunk of related
-- events with a contextual signature (cwd, intent, time bracket)
-- that an agent can retrieve instance-by-instance. RAG/in-context/
-- parametric memory each cover at most three of the five episodic
-- properties (long-term, explicit, single-shot, instance-specific,
-- contextually-related); none covers all five.
--
-- aichronicles ingests events (too granular) and aggregates them
-- into sessions (too coarse and not contextually-bound — a long
-- session can span multiple intents and cwds). The episodes table
-- sits between: a contiguous run of events sharing a cwd and a
-- coherent intent, bounded by idle gaps. Populated by a segmenter
-- that walks each session's ordered events; per-session ordinal
-- gives a stable retrieval handle ("show me the third episode in
-- session abc12345").
--
-- Schema:
--   id              autoincrement PK.
--   session_id      FK to sessions(id), CASCADE on delete.
--   ordinal         1-based position within the parent session
--                   (a session always has at least episode 1).
--   started_at_ms   first event's ts_source_ms in this episode.
--   ended_at_ms     last event's ts_source_ms in this episode.
--   cwd             the running cwd for this episode (NULL if
--                   no event in the episode carried a cwd).
--   intent_summary  short prose summary derived from the first
--                   user_prompt in the episode (empty when no
--                   user_prompt landed).
--   event_count     how many events ended up in this episode.
--   first_event_id  the chronologically-first event_id in the
--                   episode — gives a deterministic anchor for
--                   future re-segmentation against the same data.
--
-- (session_id, ordinal) is naturally unique. Enforce via UNIQUE.
-- The segmenter is idempotent: re-running on the same events
-- produces the same boundaries; a re-population path will use
-- DELETE-then-INSERT under a transaction rather than UPDATE.

CREATE TABLE episodes (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal         INTEGER NOT NULL,
    started_at_ms   INTEGER NOT NULL,
    ended_at_ms     INTEGER NOT NULL,
    cwd             TEXT,
    intent_summary  TEXT NOT NULL DEFAULT '',
    event_count     INTEGER NOT NULL,
    first_event_id  TEXT NOT NULL REFERENCES events(event_id) ON DELETE CASCADE,
    UNIQUE(session_id, ordinal)
);

-- Read paths: "all episodes for a session, in order" (ordinal asc),
-- plus "all episodes touching a given cwd in a window" for the
-- future cwd-scoped retrieval the synthesis recommends.
CREATE INDEX idx_episodes_session_ordinal
    ON episodes(session_id, ordinal);
CREATE INDEX idx_episodes_cwd_started
    ON episodes(cwd, started_at_ms DESC)
    WHERE cwd IS NOT NULL;

UPDATE meta SET value='25' WHERE key='schema_version';
```

## `026_episodes_cwd_ended_index.sql`

```sql
-- Migration 026: realign episodes' cwd-scoped index to match the
-- ORDER BY in store.FindEpisodes.
--
-- Migration 025 created `idx_episodes_cwd_started ON episodes(cwd,
-- started_at_ms DESC) WHERE cwd IS NOT NULL`, anticipating a
-- "started_at_ms-ordered" cwd-scoped retrieval that ended up not
-- being the actual sort key. FindEpisodes orders by `ended_at_ms
-- DESC, id DESC`, so the started-anchored index can only help with
-- the cwd= equality predicate; SQLite still has to materialise the
-- matching set and re-sort it. For a recall query that's
-- consistently LIMIT-bounded ("most recent N episodes in this
-- project"), that's a hot-path inefficiency.
--
-- Drop the old index and create one anchored on `ended_at_ms DESC`,
-- with `id DESC` baked in so the planner can satisfy ORDER BY...
-- LIMIT directly from the index without a sort step.
--
-- The compound index is still partial (WHERE cwd IS NOT NULL): rows
-- with NULL cwd never participate in cwd= filters, so excluding them
-- keeps the index small.
DROP INDEX IF EXISTS idx_episodes_cwd_started;
CREATE INDEX idx_episodes_cwd_ended
    ON episodes(cwd, ended_at_ms DESC, id DESC)
    WHERE cwd IS NOT NULL;

UPDATE meta SET value='26' WHERE key='schema_version';
```
