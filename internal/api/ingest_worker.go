package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
// the pre-async path), and SSE bus. A nil log falls back to a
// discard handler so callers don't need a per-site guard.
func NewIngestWorker(s *store.Store, pipeline events.Pipeline, bus *sseBus, log *slog.Logger) *IngestWorker {
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

// drain processes one FIFO batch of pending rows. One row at a
// time per Pipeline.Process call — see the IngestWorker doc
// comment for the failure-isolation rationale.
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
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.processOne(ctx, row)
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
