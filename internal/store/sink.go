// SQLite implementations of events.Sink. Sink (single-tx-per-call)
// powers the daemon's HTTP path; BufferedSink (chunked commits with
// row-by-row fallback) powers importers. Both share
// IngestEnvelopeWithExtractions for the actual SQL.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
)

// Sink writes one envelope per call in its own SQLite transaction.
// Designed for the daemon HTTP path where one request = one event
// and per-request atomicity matches the request lifecycle. Tx
// failures bubble up to the caller (typically translated to
// HTTP 5xx).
//
// Flush is a no-op; Close is a no-op (the *Store is owned by the
// caller). Stats accumulates Imported / Deduped counts across all
// Writes so the Pipeline can report them at end-of-run.
type Sink struct {
	store *Store
	now   func() time.Time

	imported int
	deduped  int
}

// NewSink wraps a *Store as an events.Sink with one-tx-per-Write
// semantics. now is injectable for tests; nil falls back to
// time.Now (UTC).
func NewSink(s *Store) *Sink {
	return &Sink{store: s, now: defaultNow}
}

// WithNow overrides the time source. Returns the receiver for
// chaining. Tests use this to pin ts_server_ms; production never
// overrides.
func (s *Sink) WithNow(now func() time.Time) *Sink {
	s.now = now
	return s
}

// Write implements events.Sink. Begins a transaction, calls
// IngestEnvelopeWithExtractions with the Event's pre-computed
// extractions, commits. Returns Result with EventID + derived
// SessionID + Deduped flag.
func (s *Sink) Write(ctx context.Context, e events.Event) (events.Result, error) {
	if e.Envelope == nil {
		return events.Result{}, errors.New("Sink.Write: nil envelope")
	}
	var deduped bool
	if err := WithTx(ctx, s.store.DB(), func(tx *sql.Tx) error {
		tsMs := s.now().UnixMilli()
		d, err := IngestEnvelopeWithExtractions(ctx, tx, e.Envelope, e.Raw, tsMs, e.Extractions)
		if err != nil {
			return err
		}
		deduped = d
		return nil
	}); err != nil {
		return events.Result{}, err
	}
	if deduped {
		s.deduped++
	} else {
		s.imported++
	}
	return events.Result{
		EventID:   e.Envelope.EventID,
		SessionID: events.DeriveSessionID(e.Envelope.SourceAgent, e.Envelope.SourceSessionID),
		Deduped:   deduped,
	}, nil
}

// Flush is a no-op for the single-tx Sink.
func (s *Sink) Flush(_ context.Context) error { return nil }

// Close is a no-op; the *Store is owned by the caller.
func (s *Sink) Close(_ context.Context) error { return nil }

// Stats returns the running totals. Single-tx sinks update on every
// successful Write.
func (s *Sink) Stats() events.SinkStats {
	return events.SinkStats{Imported: s.imported, Deduped: s.deduped}
}

// BufferedSinkOpts tunes when BufferedSink auto-flushes. Both caps
// are inclusive: hitting either triggers a flush. Zero values fall
// back to the package defaults (DefaultBufferedSinkRows /
// DefaultBufferedSinkBytes), the same thresholds the previous
// envelopeBatcher used.
type BufferedSinkOpts struct {
	MaxRows  int
	MaxBytes int
}

// DefaultBufferedSinkRows caps how many envelopes ride a single
// import transaction. SQLite's per-commit fsync was the dominant
// cost in the pre-batching importer (~410 envelopes/sec on a real-
// world transcript dump); amortising one fsync over ~1000 rows
// brings throughput into the 5-15K/sec range.
const DefaultBufferedSinkRows = 1000

// DefaultBufferedSinkBytes caps the cumulative envelope_json size
// per transaction. Claude transcripts can carry the occasional
// multi-tens-of-MB tool-result line; 32 MB keeps a steady-state
// importer's RAM bounded.
const DefaultBufferedSinkBytes = 32 << 20

// BufferedSink is an events.Sink that amortises SQLite fsync cost
// across many envelopes by holding up to MaxRows / MaxBytes events
// in memory and committing them in one transaction.
//
// Two-phase semantics — preserves the contract envelopeBatcher had:
//
//   - fast path: a chunk of buffered envelopes commits in a single
//     tx; one fsync for the whole batch.
//   - fallback: if the chunk tx errors anywhere (FK violation, an
//     unredacted envelope sneaking in, a SQLite-level fault), the
//     chunk is rolled back and replayed row-by-row in individual
//     transactions. A single broken envelope still aborts the
//     remaining work — same as pre-batching — but only after
//     surrounding rows have had their own chance.
//
// Not safe for concurrent Write — one BufferedSink per import
// goroutine.
type BufferedSink struct {
	store    *Store
	maxRows  int
	maxBytes int
	now      func() time.Time

	buf      []pendingWrite
	bufBytes int

	// Running totals updated during Flush. The Pipeline cannot
	// derive dedup counts from per-Write Results (the sink buffers
	// the actual SQL until Flush, so Write returns Deduped=false
	// even when the row would dedupe on commit). Callers reading
	// final import stats use Imported() / Deduped() instead.
	imported int
	deduped  int
}

type pendingWrite struct {
	event events.Event
	tsMs  int64
}

// NewBufferedSink returns a BufferedSink with the provided thresholds
// or the package defaults when a field is zero.
func NewBufferedSink(s *Store, opts BufferedSinkOpts) *BufferedSink {
	rows := opts.MaxRows
	if rows <= 0 {
		rows = DefaultBufferedSinkRows
	}
	bytes := opts.MaxBytes
	if bytes <= 0 {
		bytes = DefaultBufferedSinkBytes
	}
	return &BufferedSink{
		store:    s,
		maxRows:  rows,
		maxBytes: bytes,
		now:      defaultNow,
	}
}

// WithNow overrides the time source for tests.
func (b *BufferedSink) WithNow(now func() time.Time) *BufferedSink {
	b.now = now
	return b
}

// Write buffers the event. Auto-flushes when the row or byte
// threshold is hit; otherwise returns a synthetic Result with
// EventID + derived SessionID and Deduped=false (the actual dedup
// outcome isn't known until the buffered batch commits, but
// callers of BufferedSink only consume aggregate Stats from
// Pipeline.Run which derives counts from Flush's running totals).
func (b *BufferedSink) Write(ctx context.Context, e events.Event) (events.Result, error) {
	if e.Envelope == nil {
		return events.Result{}, errors.New("BufferedSink.Write: nil envelope")
	}
	b.buf = append(b.buf, pendingWrite{
		event: e,
		tsMs:  b.now().UnixMilli(),
	})
	b.bufBytes += len(e.Raw)
	result := events.Result{
		EventID:   e.Envelope.EventID,
		SessionID: events.DeriveSessionID(e.Envelope.SourceAgent, e.Envelope.SourceSessionID),
	}
	if len(b.buf) >= b.maxRows || b.bufBytes >= b.maxBytes {
		if err := b.Flush(ctx); err != nil {
			return events.Result{}, err
		}
	}
	return result, nil
}

// Flush commits the buffered batch with row-by-row fallback on
// transaction failure. Idempotent on an empty buffer.
func (b *BufferedSink) Flush(ctx context.Context) error {
	if len(b.buf) == 0 {
		return nil
	}
	pending := b.buf
	b.buf = nil
	b.bufBytes = 0

	if err := b.flushBatch(ctx, pending); err != nil {
		// Fallback: replay one envelope per tx so a single bad
		// row does not reject the rest of the chunk. Per-row
		// failures still propagate (matches envelopeBatcher).
		return b.flushRowByRow(ctx, pending)
	}
	return nil
}

// Close flushes any remaining buffered events under the provided
// ctx. Cancelling ctx aborts the final drain.
func (b *BufferedSink) Close(ctx context.Context) error {
	return b.Flush(ctx)
}

// Stats returns the running totals. Buffered sinks update on each
// Flush (not on Write); callers that need authoritative counts
// must Flush before reading.
func (b *BufferedSink) Stats() events.SinkStats {
	return events.SinkStats{Imported: b.imported, Deduped: b.deduped}
}

// flushBatch attempts the entire chunk in one tx. Returns the first
// error encountered; the caller falls back to flushRowByRow.
func (b *BufferedSink) flushBatch(ctx context.Context, pending []pendingWrite) error {
	var imported, deduped int
	if err := WithTx(ctx, b.store.DB(), func(tx *sql.Tx) error {
		for _, p := range pending {
			d, err := IngestEnvelopeWithExtractions(ctx, tx, p.event.Envelope, p.event.Raw, p.tsMs, p.event.Extractions)
			if err != nil {
				return fmt.Errorf("ingest %s: %w", p.event.Envelope.EventID, err)
			}
			if d {
				deduped++
			} else {
				imported++
			}
		}
		return nil
	}); err != nil {
		return err
	}
	b.imported += imported
	b.deduped += deduped
	return nil
}

// flushRowByRow replays a chunk one envelope per transaction. Used
// as the batch fallback so a single bad envelope does not take down
// the others. Updates running totals row-by-row so callers see what
// landed before a per-row error stopped the rest.
func (b *BufferedSink) flushRowByRow(ctx context.Context, pending []pendingWrite) error {
	for _, p := range pending {
		var deduped bool
		if err := WithTx(ctx, b.store.DB(), func(tx *sql.Tx) error {
			d, err := IngestEnvelopeWithExtractions(ctx, tx, p.event.Envelope, p.event.Raw, p.tsMs, p.event.Extractions)
			if err != nil {
				return err
			}
			deduped = d
			return nil
		}); err != nil {
			return err
		}
		if deduped {
			b.deduped++
		} else {
			b.imported++
		}
	}
	return nil
}

func defaultNow() time.Time { return time.Now().UTC() }
