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
