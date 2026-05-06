package events

import "context"

// Sink consumes events. Implementations decide their own atomicity
// and batching: one-tx-per-call (live ingest) or buffered-and-
// flushed (batch import) is up to the Sink. pkg/events does NOT
// import database/sql; SQLite-backed Sinks live in internal/store
// and satisfy this interface.
//
// Lifecycle:
//   - Write may be called many times.
//   - Flush forces any buffered writes to commit; idempotent and
//     safe to call between Write batches. A non-buffered Sink
//     implements Flush as a no-op.
//   - Close flushes and releases resources (close DB handles,
//     drop file descriptors). After Close, the Sink is unusable.
//     The ctx bounds any final flush; cancel it to abort an
//     in-progress drain on shutdown.
//   - Stats returns running totals reflecting WHAT HAS COMMITTED
//     so far. Single-tx sinks update on each Write; buffered
//     sinks update on Flush. Pipeline.Run reads Sink.Stats() at
//     end-of-run to populate Pipeline.Stats.Processed/Deduped —
//     this is the only reliable path for the buffered case
//     because Result.Deduped from Write is necessarily false
//     pre-flush.
//
// Pipeline.Run calls Flush at the end of a successful run and
// expects callers to defer Close on their Sink instance.
type Sink interface {
	Write(ctx context.Context, e Event) (Result, error)
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
	Stats() SinkStats
}

// SinkStats is the aggregate persistence outcome a Sink reports.
// Imported counts envelopes that resulted in an INSERT (a new
// row); Deduped counts envelopes whose event_id already existed
// (the no-op INSERT OR IGNORE path). Imported + Deduped is the
// total accepted by the Sink (errors are not counted here — they
// surface via Sink.Write returning a non-nil error).
type SinkStats struct {
	Imported int
	Deduped  int
}
