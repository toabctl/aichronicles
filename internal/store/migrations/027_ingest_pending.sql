-- Two-phase ingest staging table.
--
-- Phase 1 (request goroutine, fast): handleIngest validates the envelope
-- shape, INSERTs into this table in a tiny tx, returns 200. No
-- redaction, no extractors, no FTS work — the hook returns in tens of
-- ms regardless of envelope size.
--
-- Phase 2 (worker goroutine, slow): the IngestWorker drains pending rows
-- in FIFO order, runs the full events.Pipeline.Process (redact +
-- extract + sink.Write into events/raw_envelopes/extractions), then
-- DELETEs the row in the same transaction that commits the event. On
-- failure the row stays and attempt_count is bumped; persistent
-- failures are surfaced via slog.Error.
--
-- Crash safety: a row only leaves this table inside the same tx that
-- commits its derived rows downstream, so a daemon crash anywhere
-- before that commit leaves the row for replay on next startup. The
-- worker runs an initial drain pass before the listener opens.
--
-- Dedup: event_id is UNIQUE so a hook that retries a previously
-- accepted (but unprocessed) event gets a "deduped" response without
-- the daemon doing any extra work.
CREATE TABLE ingest_pending (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id            TEXT NOT NULL UNIQUE,   -- envelope.event_id; dedup at phase-1
    body                BLOB NOT NULL,           -- raw POST body bytes, pre-redaction
    received_at_ms      INTEGER NOT NULL,        -- daemon receipt timestamp
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    last_attempt_at_ms  INTEGER,                 -- NULL until the worker has tried once
    last_error          TEXT                     -- short stringified error from the last failed attempt
);

-- FIFO drain order: oldest received_at_ms first. Index covers both
-- the worker's batch SELECT and the "how far behind are we?" admin
-- queries.
CREATE INDEX idx_ingest_pending_received ON ingest_pending(received_at_ms);

UPDATE meta SET value='27' WHERE key='schema_version';
