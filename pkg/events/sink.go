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
//
// Pipeline.Run calls Flush at the end of a successful run and
// expects callers to defer Close on their Sink instance.
type Sink interface {
	Write(ctx context.Context, e Event) (Result, error)
	Flush(ctx context.Context) error
	Close() error
}
