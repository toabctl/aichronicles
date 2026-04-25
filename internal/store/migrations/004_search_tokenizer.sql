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
