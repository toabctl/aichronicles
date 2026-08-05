-- Terminal state for pending rows that can never be processed.
--
-- ingest_pending stores the raw POST body PRE-redaction (see the
-- column comment in 027) because redaction happens in the worker, not
-- at the accept boundary. That is the right trade for durability —
-- the daemon acks only after the bytes are on disk — but it makes a
-- row that never drains a permanent plaintext liability.
--
-- Two failure modes made that reachable:
--
--   * The accept path used json.Decoder.Decode, which ignores bytes
--     after the first JSON value, while the worker used
--     json.Unmarshal, which rejects them. A body with trailing junk
--     was therefore 200-acked and enqueued, then failed forever.
--   * recordFailure only ever incremented attempt_count. Nothing
--     removed a row, and PendingBatch is strict FIFO, so a full batch
--     of poison rows starved every healthy row behind it. The queue
--     wedged permanently and no restart cleared it.
--
-- This table is the terminal state. A row that exhausts max_attempts
-- is summarised here and DELETEd from ingest_pending, which unblocks
-- the queue and drops the unredacted body in the same step.
--
-- Deliberately NO body column. The operator needs to know that an
-- event was dropped and why; keeping the bytes would reintroduce
-- exactly the liability this migration exists to end. CLAUDE.md §7:
-- capture the bare fact, drop what you cannot store safely.
--
-- last_error is already truncated to 512 chars by MarkPendingFailed
-- and is a decoder message ("unmarshal: invalid character ..."), not
-- payload — it names the syntax problem, not the content.

CREATE TABLE ingest_dead_letter (
    id                 INTEGER PRIMARY KEY,
    event_id           TEXT    NOT NULL,
    received_at_ms     INTEGER NOT NULL,
    dead_lettered_at_ms INTEGER NOT NULL,
    attempt_count      INTEGER NOT NULL,
    last_error         TEXT
);

-- Retention/inspection both scan by time, and the admin stats handler
-- wants "how many recently".
CREATE INDEX idx_ingest_dead_letter_time
    ON ingest_dead_letter(dead_lettered_at_ms);

-- event_id is NOT unique: the same event can legitimately be
-- re-POSTed by a retrying hook, fail again, and be dead-lettered
-- again. Uniqueness here would turn a second failure into an
-- INSERT error inside the worker's cleanup path — the last place
-- that should be able to fail.
CREATE INDEX idx_ingest_dead_letter_event ON ingest_dead_letter(event_id);

UPDATE meta SET value='30' WHERE key='schema_version';
