-- Migration 029: enforce evidence-quote invariant on semantic_facts.
--
-- SaveSemanticFact (commit b86fa9f) requires evidence_quote to be
-- non-empty whenever evidence_session_id is set: the quote is the
-- substrate the user (or LLM) can re-verify against the cited
-- session. The Go check closed half the loop; the table itself
-- placed no constraint on the pair, so a direct INSERT, a future
-- writer, or a migration that forgets the rule could record a
-- fact whose session pointer has no grounding excerpt — the
-- fabrication path the review at 2026-05-14 flagged.
--
-- SQLite doesn't accept ALTER TABLE ADD CHECK; rebuild the table
-- with the constraint baked in. Nothing FK-references
-- semantic_facts.id so we don't need the
-- PRAGMA foreign_keys=OFF dance.
--
-- Pre-existing rows that violate the new invariant (session
-- pointer set, quote empty/NULL) keep their fact data but lose
-- the session pointer in the copy. The fact remains claimed by
-- the source llm_output; the row just becomes non-drillable to a
-- session — same as the migration-019 contract for pruned
-- sessions. Better to drop the pointer than to grandfather in a
-- row that asserts a quote it can't produce.

CREATE TABLE semantic_facts_new (
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
    UNIQUE(subject, predicate, object),
    CHECK (evidence_session_id IS NULL
           OR (evidence_quote IS NOT NULL AND evidence_quote != ''))
);

INSERT INTO semantic_facts_new
    SELECT id, source_llm_output_id, subject, predicate, object,
           confidence,
           CASE
             WHEN evidence_quote IS NULL OR evidence_quote = ''
             THEN NULL
             ELSE evidence_session_id
           END,
           evidence_quote,
           asserted_at_ms
      FROM semantic_facts;

DROP TABLE semantic_facts;
ALTER TABLE semantic_facts_new RENAME TO semantic_facts;

-- DROP TABLE removes the indexes too — recreate matching 019.
CREATE INDEX idx_semantic_facts_subject ON semantic_facts(subject, predicate);
CREATE INDEX idx_semantic_facts_recent ON semantic_facts(asserted_at_ms DESC);

UPDATE meta SET value='29' WHERE key='schema_version';
