package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
)

// makeEnv builds a redaction-applied envelope that IngestEnvelope
// will accept. Used by every batcher test that wants a "good" row.
func makeEnv(t *testing.T, kind string) *events.Envelope {
	t.Helper()
	return &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-batch",
		Kind:            kind,
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Cwd:             "/tmp/batch",
		ContentText:     "content " + kind,
		Payload:         map[string]any{"k": kind},
		Redaction:       &events.Redaction{Applied: true},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func countEvents(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func TestBatcher_FlushOnRowCap(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	b := newEnvelopeBatcherForTest(s, 3, 1<<30) // 3-row cap, byte cap effectively off

	for range 3 {
		env := makeEnv(t, "user_prompt")
		if err := b.Add(t.Context(), env, mustJSON(t, env)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// After 3 Adds, the batcher must have flushed automatically;
	// the events should already be queryable without an explicit
	// Flush call.
	if got := countEvents(t, s); got != 3 {
		t.Errorf("auto-flush at row cap: got %d events in DB, want 3", got)
	}
	if b.Imported() != 3 || b.Deduped() != 0 {
		t.Errorf("counts after row-cap flush: imported=%d deduped=%d", b.Imported(), b.Deduped())
	}
}

func TestBatcher_FlushOnByteCap(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	// Row cap effectively off; byte cap at ~10KB.
	b := newEnvelopeBatcherForTest(s, 1_000_000, 10_000)

	bigPayload := strings.Repeat("x", 6_000) // each row ~6KB scrubbed
	for range 3 {
		env := makeEnv(t, "user_prompt")
		env.ContentText = bigPayload
		if err := b.Add(t.Context(), env, mustJSON(t, env)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// Two rows × ~6KB > 10KB byte cap → must have auto-flushed.
	// All three should now be visible (the third either flushed
	// with the first two or stayed buffered; force the last one
	// to land via Flush).
	if err := b.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := countEvents(t, s); got != 3 {
		t.Errorf("byte-cap flush: got %d events, want 3", got)
	}
}

func TestBatcher_ManualFlushOnPartialBuffer(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	b := newEnvelopeBatcherForTest(s, 1000, 1<<30)

	env := makeEnv(t, "user_prompt")
	if err := b.Add(t.Context(), env, mustJSON(t, env)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// One row, neither cap hit — must not have auto-flushed.
	if got := countEvents(t, s); got != 0 {
		t.Errorf("partial buffer should not auto-flush; got %d events", got)
	}
	if err := b.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := countEvents(t, s); got != 1 {
		t.Errorf("after Flush: got %d events, want 1", got)
	}
}

func TestBatcher_FlushOnEmptyBufferIsNoop(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	b := newEnvelopeBatcher(s)
	if err := b.Flush(t.Context()); err != nil {
		t.Errorf("empty Flush returned error: %v", err)
	}
	if b.Imported() != 0 || b.Deduped() != 0 {
		t.Error("empty Flush incremented counters")
	}
}

func TestBatcher_DedupesWithinBatch(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	b := newEnvelopeBatcherForTest(s, 100, 1<<30)

	env := makeEnv(t, "user_prompt")
	rawA := mustJSON(t, env)
	if err := b.Add(t.Context(), env, rawA); err != nil {
		t.Fatalf("Add A: %v", err)
	}
	// Same event_id, second occurrence inside the same chunk must
	// be classified as deduped (INSERT OR IGNORE inside one tx).
	envDup := *env
	if err := b.Add(t.Context(), &envDup, rawA); err != nil {
		t.Fatalf("Add A-dup: %v", err)
	}
	if err := b.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if b.Imported() != 1 {
		t.Errorf("Imported: got %d, want 1", b.Imported())
	}
	if b.Deduped() != 1 {
		t.Errorf("Deduped: got %d, want 1", b.Deduped())
	}
	if got := countEvents(t, s); got != 1 {
		t.Errorf("events table: got %d, want 1 (dedup must collapse to one row)", got)
	}
}

func TestImportBatch_FailsWholeChunkOnBadRow(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	good1 := makeEnv(t, "user_prompt")
	bad := makeEnv(t, "user_prompt")
	bad.Redaction = &events.Redaction{Applied: false} // tickles ErrRedactionRequired
	good2 := makeEnv(t, "tool_use")

	batch := []pendingEnvelope{
		{env: good1, raw: mustJSON(t, good1), tsMs: time.Now().UnixMilli()},
		{env: bad, raw: mustJSON(t, bad), tsMs: time.Now().UnixMilli()},
		{env: good2, raw: mustJSON(t, good2), tsMs: time.Now().UnixMilli()},
	}
	imported, deduped, err := importBatch(t.Context(), s, batch)
	if err == nil {
		t.Fatal("importBatch must fail when a row carries Redaction.Applied=false")
	}
	if !errors.Is(err, store.ErrRedactionRequired) {
		t.Errorf("wrapped error should be ErrRedactionRequired, got %v", err)
	}
	if imported != 0 || deduped != 0 {
		t.Errorf("on batch failure expected zero counts, got imported=%d deduped=%d",
			imported, deduped)
	}
	// Critically: the batch transaction was rolled back, so good1
	// must NOT have been persisted yet — that's what the row-by-row
	// fallback is for.
	if got := countEvents(t, s); got != 0 {
		t.Errorf("batch rollback failed: %d events landed despite the error", got)
	}
}

func TestImportRowByRow_IsolatesBadRowAndCommitsRest(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	good1 := makeEnv(t, "user_prompt")
	bad := makeEnv(t, "user_prompt")
	bad.Redaction = &events.Redaction{Applied: false}
	good2 := makeEnv(t, "tool_use")

	batch := []pendingEnvelope{
		{env: good1, raw: mustJSON(t, good1), tsMs: time.Now().UnixMilli()},
		{env: bad, raw: mustJSON(t, bad), tsMs: time.Now().UnixMilli()},
		{env: good2, raw: mustJSON(t, good2), tsMs: time.Now().UnixMilli()},
	}
	imported, deduped, err := importRowByRow(t.Context(), s, batch)
	if err == nil {
		t.Fatal("importRowByRow should propagate the bad row's error")
	}
	if !errors.Is(err, store.ErrRedactionRequired) {
		t.Errorf("wrapped error should be ErrRedactionRequired, got %v", err)
	}
	// good1 committed in its own tx BEFORE we hit the bad row;
	// good2 was never reached. The whole point of the fallback is
	// that the rows preceding the bad one are NOT lost.
	if imported != 1 || deduped != 0 {
		t.Errorf("row-by-row counts: got imported=%d deduped=%d, want 1/0",
			imported, deduped)
	}
	if got := countEvents(t, s); got != 1 {
		t.Errorf("row-by-row should have persisted good1; got %d events", got)
	}
}

func TestBatcher_FallbackPath_PartialFailureCountsAccurate(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	// Tiny chunk so we can predict exactly when Flush fires.
	b := newEnvelopeBatcherForTest(s, 5, 1<<30)

	good1 := makeEnv(t, "user_prompt")
	good2 := makeEnv(t, "tool_use")
	bad := makeEnv(t, "user_prompt")
	bad.Redaction = &events.Redaction{Applied: false}
	good3 := makeEnv(t, "assistant_message") // never reached

	if err := b.Add(t.Context(), good1, mustJSON(t, good1)); err != nil {
		t.Fatalf("Add good1: %v", err)
	}
	if err := b.Add(t.Context(), good2, mustJSON(t, good2)); err != nil {
		t.Fatalf("Add good2: %v", err)
	}
	if err := b.Add(t.Context(), bad, mustJSON(t, bad)); err != nil {
		t.Fatalf("Add bad: %v", err)
	}
	// Adding good3 doesn't auto-flush (4 < 5), so we'll Flush
	// explicitly; the fallback should commit good1+good2 and
	// then propagate the failure on bad — leaving good3 in the
	// buffer (not yet attempted).
	if err := b.Add(t.Context(), good3, mustJSON(t, good3)); err != nil {
		t.Fatalf("Add good3: %v", err)
	}

	err := b.Flush(t.Context())
	if err == nil {
		t.Fatal("Flush should propagate the bad-row error")
	}
	if !errors.Is(err, store.ErrRedactionRequired) {
		t.Errorf("expected ErrRedactionRequired, got %v", err)
	}
	if b.Imported() != 2 || b.Deduped() != 0 {
		t.Errorf("counts after partial fallback: imported=%d deduped=%d (want 2/0)",
			b.Imported(), b.Deduped())
	}
	if got := countEvents(t, s); got != 2 {
		t.Errorf("DB should hold the rows preceding the bad one; got %d", got)
	}
}
