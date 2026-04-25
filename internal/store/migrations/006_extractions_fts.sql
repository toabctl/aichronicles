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
