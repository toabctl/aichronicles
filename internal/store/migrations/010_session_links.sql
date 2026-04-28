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
