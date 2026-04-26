package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// importBatchRows caps how many envelopes ride a single import
// transaction. SQLite's per-commit fsync was the dominant cost in
// the pre-batching importer (~410 envelopes/sec on a real-world
// transcript dump); amortising one fsync over ~1000 rows brings
// throughput into the 5-15K/sec range without changing the
// per-row write set or any trigger semantics.
const importBatchRows = 1000

// importBatchBytes caps the cumulative envelope_json size per
// transaction. Claude transcripts can carry the occasional
// multi-tens-of-MB tool-result line (the existing
// maxClaudeLineBytes = 128 MB constant testifies to it); without a
// byte cap a 1000-row buffer of those would hold GBs before
// flushing. 32 MB keeps a steady-state importer's RAM bounded.
const importBatchBytes = 32 << 20

// pendingEnvelope holds one envelope waiting on the batcher. tsMs
// is captured at Add time, not at flush time, so each row's
// ts_server_ms reflects "when the importer saw the line" — the
// same semantic the per-row pre-batching code carried, just no
// longer coupled to commit cadence.
type pendingEnvelope struct {
	env  *ingest.Envelope
	raw  []byte
	tsMs int64
}

// envelopeBatcher amortises SQLite fsync cost across many envelopes
// while preserving the pre-batching contract that one bad row
// never aborts the whole import.
//
// Two-phase semantics:
//
//   - fast path: a chunk of up to importBatchRows envelopes (or
//     importBatchBytes total payload) commits in a single
//     transaction. On success that's one fsync for the whole
//     chunk.
//   - fallback: if the chunk transaction errors anywhere
//     (storage-level failure, FK violation, an envelope that
//     somehow slipped through without Redaction.Applied=true)
//     the chunk is rolled back and replayed row-by-row in
//     individual transactions. A single broken envelope still
//     aborts the import — same as pre-batching — but only after
//     the surrounding rows have been given their own chance.
//
// Counts of imported / deduped envelopes are aggregated across
// flushes. The caller is responsible for invoking Flush at
// end-of-input so the trailing partial chunk lands.
//
// Not safe for concurrent Add — instantiate one batcher per
// import goroutine.
type envelopeBatcher struct {
	s          *store.Store
	batchRows  int
	batchBytes int

	buf      []pendingEnvelope
	bufBytes int

	imported int
	deduped  int
}

// newEnvelopeBatcher returns a batcher with the package defaults.
// Tests use newEnvelopeBatcherForTest to dial down the thresholds
// without recompiling.
func newEnvelopeBatcher(s *store.Store) *envelopeBatcher {
	return &envelopeBatcher{
		s:          s,
		batchRows:  importBatchRows,
		batchBytes: importBatchBytes,
	}
}

// newEnvelopeBatcherForTest exposes the row / byte thresholds so
// tests can force the auto-flush boundary without seeding a
// thousand fixtures. Production callers always go through
// newEnvelopeBatcher.
func newEnvelopeBatcherForTest(s *store.Store, batchRows, batchBytes int) *envelopeBatcher {
	if batchRows <= 0 {
		batchRows = importBatchRows
	}
	if batchBytes <= 0 {
		batchBytes = importBatchBytes
	}
	return &envelopeBatcher{
		s:          s,
		batchRows:  batchRows,
		batchBytes: batchBytes,
	}
}

// Add queues one envelope. When the buffer hits either the row
// cap or the byte cap, Add transparently flushes — so callers
// stream their input through Add and only need an explicit Flush
// at end-of-input.
func (b *envelopeBatcher) Add(ctx context.Context, env *ingest.Envelope, raw []byte) error {
	b.buf = append(b.buf, pendingEnvelope{
		env:  env,
		raw:  raw,
		tsMs: time.Now().UTC().UnixMilli(),
	})
	b.bufBytes += len(raw)
	if len(b.buf) >= b.batchRows || b.bufBytes >= b.batchBytes {
		return b.Flush(ctx)
	}
	return nil
}

// Flush commits the buffered envelopes, falling back to row-by-row
// if the chunk transaction fails. Idempotent on an empty buffer.
//
// Counts are folded into the batcher's running totals even on a
// partial-failure fallback — that way Imported / Deduped reported
// to the caller's report struct reflect the rows that DID land
// before the bad envelope was hit, not just the all-clean path.
func (b *envelopeBatcher) Flush(ctx context.Context) error {
	if len(b.buf) == 0 {
		return nil
	}
	pending := b.buf
	b.buf = nil
	b.bufBytes = 0

	imported, deduped, err := importBatch(ctx, b.s, pending)
	if err != nil {
		// Fallback path: replay the chunk one envelope at a time
		// so a single bad row doesn't reject the rest of the
		// chunk. Per-row failures still propagate (the
		// pre-batching contract); accumulate whatever the
		// fallback DID manage to commit so the caller's report
		// matches what's on disk.
		imported, deduped, err = importRowByRow(ctx, b.s, pending)
	}
	b.imported += imported
	b.deduped += deduped
	return err
}

// Imported / Deduped expose the running totals after Flush, for
// the caller's report struct.
func (b *envelopeBatcher) Imported() int { return b.imported }
func (b *envelopeBatcher) Deduped() int  { return b.deduped }

// importBatch attempts the full chunk in one transaction. Returns
// (imported, deduped, nil) on success — that's one fsync amortised
// over len(batch) envelopes. On any per-row or commit error,
// returns zero counts and the error so the caller can fall back.
func importBatch(ctx context.Context, s *store.Store, batch []pendingEnvelope) (int, int, error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var imported, deduped int
	for _, p := range batch {
		d, err := store.IngestEnvelope(ctx, tx, p.env, p.raw, p.tsMs)
		if err != nil {
			return 0, 0, fmt.Errorf("ingest %s: %w", p.env.EventID, err)
		}
		if d {
			deduped++
		} else {
			imported++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return imported, deduped, nil
}

// importRowByRow replays a chunk one envelope per transaction. Used
// as the batch fallback so a single bad envelope doesn't take down
// 999 healthy ones. Storage-level errors still propagate — same
// contract as before batching.
func importRowByRow(ctx context.Context, s *store.Store, batch []pendingEnvelope) (int, int, error) {
	var imported, deduped int
	for _, p := range batch {
		d, err := importOne(ctx, s, p.env, p.raw, p.tsMs)
		if err != nil {
			return imported, deduped, err
		}
		if d {
			deduped++
		} else {
			imported++
		}
	}
	return imported, deduped, nil
}

// importOne runs ONE envelope insertion in its own transaction.
// The only per-row writer in the package; both ImportJSONL and
// ImportClaudeTranscripts historically had their own copies
// (importOne / importOneEnvelope), now consolidated here.
func importOne(ctx context.Context, s *store.Store, env *ingest.Envelope, raw []byte, tsMs int64) (bool, error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deduped, err := store.IngestEnvelope(ctx, tx, env, raw, tsMs)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return deduped, nil
}
