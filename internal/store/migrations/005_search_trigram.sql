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
