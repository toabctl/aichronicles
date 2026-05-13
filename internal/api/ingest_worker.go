package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// Defaults for IngestWorker. Tunable on the struct but the
// production wiring in NewServer never overrides them — the
// numbers are picked for a personal-use deployment.
const (
	// defaultWorkerBatchSize caps how many pending rows the worker
	// pulls per drain pass. Small enough that one bad row doesn't
	// strand a long batch behind it, large enough that the per-pass
	// SQL round-trip cost amortises over many rows during a backlog
	// catch-up.
	defaultWorkerBatchSize = 16

	// defaultWorkerPollInterval is the heartbeat tick. Wake() is
	// the primary trigger, but a poll catches the case where a
	// wake was coalesced or missed during shutdown — the cost of a
	// 1s SELECT against an empty table is negligible.
	defaultWorkerPollInterval = 1 * time.Second

	// defaultWorkerMaxAttempts is the count after which a failing
	// row escalates its log to LevelError. The row is NOT auto-
	// deleted — the operator decides whether to manually inspect
	// or drop it. Five attempts gives transient SQLite locking,
	// scanner timeouts, etc. plenty of room to clear on their own.
	defaultWorkerMaxAttempts = 5

	// defaultWorkerShutdownBudget bounds the final-drain pass at
	// shutdown so a 50k-row backlog can't keep the daemon's
	// SIGTERM hanging. Anything not drained within this window
	// stays in ingest_pending and is picked up on next startup.
	defaultWorkerShutdownBudget = 5 * time.Second
)

// IngestWorker drains ingest_pending in the background, running the
// expensive parts of the ingest pipeline (redact + extract + write
// to events / raw_envelopes / extractions + FTS indexing) off the
// hook's critical path. The handler that accepted the envelope has
// already returned 200 by the time we touch it, so the hook is no
// longer waiting on us.
//
// The worker owns exactly one goroutine. SQLite serializes writes
// regardless of how many goroutines call it, so adding more
// workers would just add lock contention. Throughput-bound
// deployments can batch rows into a single transaction (separate
// follow-up); the current single-row-per-tx design prioritises
// failure isolation (one bad envelope can't poison a batch of
// good ones).
type IngestWorker struct {
	store    *store.Store
	pipeline events.Pipeline
	sseBus   *sseBus
	log      *slog.Logger

	batchSize      int
	pollInterval   time.Duration
	maxAttempts    int
	shutdownBudget time.Duration

	// pendingDepth points at the Server's in-memory queue-depth
	// counter. The worker decrements it after MarkPendingProcessed
	// commits so handler-side backpressure stays accurate without
	// the per-POST CountPending(*) scan.
	pendingDepth *atomic.Int64

	// wake is a 1-slot buffered channel. Handlers send a non-
	// blocking signal here after each enqueue; the worker's
	// select coalesces multiple wakes into one drain pass.
	wake chan struct{}

	// done closes when Run returns so Stop / external waits can
	// observe a clean exit without polling.
	done chan struct{}
}

// NewIngestWorker builds a worker bound to the given store, pipeline
// (typically the same Pipeline the Server uses for sync ingest in
// the pre-async path), and SSE bus. pendingDepth is the Server's
// queue-depth counter the worker decrements after each successful
// drain; a nil pointer is tolerated for tests that don't care
// about the counter. A nil log falls back to a discard handler so
// callers don't need a per-site guard.
func NewIngestWorker(s *store.Store, pipeline events.Pipeline, bus *sseBus, log *slog.Logger, pendingDepth *atomic.Int64) *IngestWorker {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &IngestWorker{
		store:          s,
		pipeline:       pipeline,
		sseBus:         bus,
		log:            log,
		batchSize:      defaultWorkerBatchSize,
		pollInterval:   defaultWorkerPollInterval,
		maxAttempts:    defaultWorkerMaxAttempts,
		shutdownBudget: defaultWorkerShutdownBudget,
		pendingDepth:   pendingDepth,
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
}

// Wake nudges the worker to drain ASAP. Non-blocking — if a wake
// is already queued, this call is a no-op. Handlers call Wake
// after each successful EnqueuePending so the worker doesn't have
// to wait for the next ticker.
func (w *IngestWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Done returns a channel that closes when Run returns. Callers
// that need to wait for the worker to finish shutdown drain
// select on this.
func (w *IngestWorker) Done() <-chan struct{} { return w.done }

// Run executes the drain loop until ctx cancels. Performs an
// initial drain pass (covering rows persisted before this Run
// started — e.g. a previous daemon crash or a shutdown that didn't
// finish draining), then loops on wake-or-tick.
//
// On ctx cancel, runs one final drain pass under a bounded
// detached context so SIGTERM can't hang forever on a huge
// backlog. Returns after the final pass; rows remaining beyond
// the budget stay in ingest_pending for next startup.
func (w *IngestWorker) Run(ctx context.Context) error {
	defer close(w.done)

	// Initial drain — covers rows left over from a prior run.
	if err := w.drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.log.Warn("ingest worker: initial drain", "err", err)
	}

	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.Background(), w.shutdownBudget)
			if err := w.drain(drainCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				w.log.Warn("ingest worker: shutdown drain", "err", err)
			}
			cancel()
			return nil
		case <-w.wake:
			if err := w.drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("ingest worker: drain", "err", err)
			}
		case <-t.C:
			if err := w.drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("ingest worker: poll drain", "err", err)
			}
		}
	}
}

// drain processes one FIFO batch of pending rows. The batch
// commits in a single transaction (one fsync amortised over up
// to batchSize rows) via processBatch; on any error the worker
// falls back to row-by-row processOne so a single broken envelope
// can't poison a batch of good ones — same two-phase semantics
// as store.BufferedSink uses for imports.
//
// drain does NOT loop internally over multiple batches: that would
// spin forever on a head-of-line failing row (PendingBatch's
// ORDER BY received_at_ms ASC would re-fetch the same row every
// iteration, and the inner loop would never see "no progress").
// Instead, drain self-Wakes when it made progress AND there's
// more pending work, so a healthy burst drains as fast as
// SQLite commits allow while a perma-failing row settles into
// one retry per heartbeat tick.
func (w *IngestWorker) drain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := store.CountPending(ctx, w.store.DB())
	if err != nil {
		return err
	}
	rows, err := store.PendingBatch(ctx, w.store.DB(), w.batchSize)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		w.processBatch(ctx, rows)
	}
	after, err := store.CountPending(ctx, w.store.DB())
	if err != nil {
		return err
	}
	// Self-wake only when both conditions hold:
	//   - the batch made measurable progress (some row got
	//     deleted), so we aren't going to spin on the same
	//     persistently-failing rows; AND
	//   - more rows remain to drain.
	// The next Run iteration will pick up the wake immediately
	// after this drain returns.
	if after < before && after > 0 {
		w.Wake()
	}
	return nil
}

// preparedEvent holds the CPU-side work a pending row needs
// before the DB tx opens: parsed envelope, redacted bytes, and
// the extraction set. Kept as a struct so the batch path can do
// all the redaction + extractor work outside the lock and then
// commit N rows in one fsync.
type preparedEvent struct {
	row      store.IngestPendingRow
	env      events.Envelope
	raw      []byte // post-redaction marshalled bytes
	extracts []events.Extraction
}

// processBatch tries to commit up to batchSize pending rows in one
// transaction (one fsync) and falls back to row-by-row processOne
// on any tx-level error. Mirrors store.BufferedSink's two-phase
// fast-path-then-row-by-row contract; per-row CPU work (parse,
// validate, redact, extract) happens before the lock so a single
// row's failure doesn't poison the batch.
//
// Failure isolation:
//  1. Parse/validate/marshal errors are recorded on the row and
//     the row is excluded from the batch. The batch proceeds with
//     the rest.
//  2. A SQL error inside the batch tx triggers a rollback and a
//     row-by-row fallback through processOne. processOne uses
//     Pipeline.Process which opens its own tx, so each row gets
//     its own success/failure verdict in the fallback path.
//
// The amortised cost: one disk fsync covers up to batchSize rows
// during a healthy burst; throughput grows roughly linearly with
// batch size until the queue empties. The fallback path costs the
// same as the pre-batching single-row drain.
func (w *IngestWorker) processBatch(ctx context.Context, rows []store.IngestPendingRow) {
	// Phase 1: CPU-only prep. Skip rows that fail validation /
	// marshalling — they record their own failure and don't ride
	// in the batch tx.
	prepared := make([]preparedEvent, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return
		}
		var env events.Envelope
		if err := json.Unmarshal(row.Body, &env); err != nil {
			w.recordFailure(ctx, row, "unmarshal: "+err.Error())
			continue
		}
		if err := env.Validate(); err != nil {
			w.recordFailure(ctx, row, "validate: "+err.Error())
			continue
		}
		w.pipeline.Redactor.Apply(&env)
		marshaled, err := json.Marshal(&env)
		if err != nil {
			w.recordFailure(ctx, row, "marshal: "+err.Error())
			continue
		}
		extracts := w.pipeline.Extractors.Run(&env)
		prepared = append(prepared, preparedEvent{
			row: row, env: env, raw: marshaled, extracts: extracts,
		})
	}
	if len(prepared) == 0 {
		return
	}

	// Phase 2: one tx for the whole batch. Each row commits its
	// downstream rows AND its dequeue in the same tx, so a crash
	// anywhere inside this WithTx leaves every row of the batch
	// untouched in ingest_pending — replay is the worker's next
	// drain pass.
	type batchResult struct {
		ingestSeq int64
		deduped   bool
		prep      *preparedEvent
	}
	results := make([]batchResult, 0, len(prepared))
	batchErr := store.WithTx(ctx, w.store.DB(), func(tx *sql.Tx) error {
		results = results[:0]
		tsMs := time.Now().UnixMilli()
		for i := range prepared {
			p := &prepared[i]
			seq, dedup, err := store.IngestEnvelopeWithExtractions(ctx, tx, &p.env, p.raw, tsMs, p.extracts)
			if err != nil {
				return fmt.Errorf("ingest %s: %w", p.env.EventID, err)
			}
			if err := store.MarkPendingProcessed(ctx, tx, p.row.ID); err != nil {
				return fmt.Errorf("mark processed %d: %w", p.row.ID, err)
			}
			results = append(results, batchResult{ingestSeq: seq, deduped: dedup, prep: p})
		}
		return nil
	})
	if batchErr != nil {
		w.log.Warn("ingest worker: batch failed, falling back to row-by-row",
			"rows", len(prepared), "err", batchErr)
		for i := range prepared {
			if err := ctx.Err(); err != nil {
				return
			}
			w.processOne(ctx, prepared[i].row)
		}
		return
	}

	// Phase 3: post-commit bookkeeping for each row that landed.
	// Counter decrement + SSE publish; same as processOne does
	// per row, just amortised here. The fan-out is deliberately
	// after the commit so a partial-commit scenario can't have
	// already-published SSE frames for rows that ended up rolled
	// back.
	now := time.Now().UnixMilli()
	for _, r := range results {
		if w.pendingDepth != nil {
			w.pendingDepth.Add(-1)
		}
		if !r.deduped {
			w.sseBus.Publish(wire.StreamEvent{
				EventID:    r.prep.env.EventID,
				SessionID:  events.DeriveSessionID(r.prep.env.SourceAgent, r.prep.env.SourceSessionID),
				IngestSeq:  r.ingestSeq,
				Kind:       r.prep.env.Kind,
				TsServerMs: now,
			})
		}
	}
}

// processOne runs one pending row through the pipeline. Crash
// safety lives in the order of operations: Pipeline.Process
// commits the event downstream BEFORE we delete from
// ingest_pending. If the daemon dies in between, the worker
// re-runs Process on next startup; raw_envelopes' UNIQUE(event_id)
// constraint absorbs the duplicate (result.Deduped=true) and the
// row finally gets deleted.
//
// Failures are recorded on the row (attempt_count + last_error)
// and the row stays for retry. The processOne caller does not
// abort the drain pass — one bad envelope shouldn't strand the
// rest of the batch.
func (w *IngestWorker) processOne(ctx context.Context, row store.IngestPendingRow) {
	var env events.Envelope
	if err := json.Unmarshal(row.Body, &env); err != nil {
		w.recordFailure(ctx, row, "unmarshal: "+err.Error())
		return
	}
	// Defense in depth: the handler also validates before
	// enqueueing, but a schema bump between enqueue and process
	// (rolling daemon upgrade with pending rows in flight) would
	// otherwise wedge a row forever. Catching it here surfaces
	// the mismatch in a single retry log line.
	if err := env.Validate(); err != nil {
		w.recordFailure(ctx, row, "validate: "+err.Error())
		return
	}
	result, err := w.pipeline.Process(ctx, events.Event{Envelope: &env, Raw: row.Body})
	if err != nil {
		w.recordFailure(ctx, row, "pipeline: "+err.Error())
		return
	}
	// Downstream commit succeeded — remove the pending row. A
	// failure here leaves the row for a retry pass; the next
	// Process call will see deduped=true via raw_envelopes
	// constraints and we'll come back to this same DELETE.
	if err := store.WithTx(ctx, w.store.DB(), func(tx *sql.Tx) error {
		return store.MarkPendingProcessed(ctx, tx, row.ID)
	}); err != nil {
		w.log.Warn("ingest worker: delete pending row after success",
			"id", row.ID, "event_id", row.EventID, "err", err)
		return
	}
	// Decrement the in-memory queue-depth counter so handler-side
	// backpressure stays accurate. Done AFTER the DELETE commits,
	// not before, so a crash between the two leaves the counter
	// agreeing with the table (over-counted by one, healed on next
	// daemon start's NewServer seed via CountPending).
	if w.pendingDepth != nil {
		w.pendingDepth.Add(-1)
	}
	// SSE publish only for newly-stored events. Re-processed
	// duplicates were already broadcast on the original ingest,
	// so re-broadcasting would double-render in every live feed.
	// IngestSeq carries the row's monotonic server-side ID — the
	// SSE handler renders it as `id: N`, and that's what a
	// reconnecting subscriber's Last-Event-ID resumes from.
	if !result.Deduped {
		w.sseBus.Publish(wire.StreamEvent{
			EventID:    result.EventID,
			SessionID:  result.SessionID,
			IngestSeq:  result.IngestSeq,
			Kind:       env.Kind,
			TsServerMs: time.Now().UnixMilli(),
		})
	}
}

// recordFailure stamps the failure on the pending row and logs at
// a level that escalates from Warn to Error once the row has hit
// maxAttempts. Operators looking for "stuck rows" should filter
// the journal on Level=ERROR + component=ingest_worker.
func (w *IngestWorker) recordFailure(ctx context.Context, row store.IngestPendingRow, msg string) {
	now := time.Now().UnixMilli()
	if err := store.MarkPendingFailed(ctx, w.store.DB(), row.ID, now, msg); err != nil {
		w.log.Error("ingest worker: mark failed", "id", row.ID, "err", err)
	}
	level := slog.LevelWarn
	if row.AttemptCount+1 >= w.maxAttempts {
		level = slog.LevelError
	}
	w.log.Log(ctx, level, "ingest worker: row failed",
		"id", row.ID,
		"event_id", row.EventID,
		"attempt", row.AttemptCount+1,
		"max_attempts", w.maxAttempts,
		"err", msg)
}
