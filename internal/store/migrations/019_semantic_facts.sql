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
