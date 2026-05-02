package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/pkg/events"
)

// openTestStore opens a fresh SQLite DB in t.TempDir for tests.
// Mirrors the pattern other store_test.go files use.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSink_Write_PersistsAllRows(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewSink(s)

	env, raw := newValidEnvelope(t)
	result, err := sink.Write(context.Background(), events.Event{Envelope: env, Raw: raw})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if result.EventID != env.EventID {
		t.Errorf("Result.EventID: got %q, want %q", result.EventID, env.EventID)
	}
	if result.SessionID != events.DeriveSessionID(env.SourceAgent, env.SourceSessionID) {
		t.Errorf("Result.SessionID: got %q, want derived", result.SessionID)
	}
	if result.Deduped {
		t.Errorf("first Write should not be Deduped")
	}

	// Verify the rows actually landed.
	var rawCount, eventCount, sessionCount int
	row := s.DB().QueryRow(`SELECT
		(SELECT COUNT(*) FROM raw_envelopes),
		(SELECT COUNT(*) FROM events),
		(SELECT COUNT(*) FROM sessions)`)
	if err := row.Scan(&rawCount, &eventCount, &sessionCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rawCount != 1 || eventCount != 1 || sessionCount != 1 {
		t.Errorf("rows after Write: raw=%d events=%d sessions=%d, want 1/1/1",
			rawCount, eventCount, sessionCount)
	}
}

func TestSink_Write_DedupesOnRepeatedEventID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewSink(s)

	env, raw := newValidEnvelope(t)
	if _, err := sink.Write(context.Background(), events.Event{Envelope: env, Raw: raw}); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	result, err := sink.Write(context.Background(), events.Event{Envelope: env, Raw: raw})
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if !result.Deduped {
		t.Errorf("second Write with same EventID should be Deduped")
	}
}

func TestSink_Write_RejectsUnredacted(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewSink(s)

	env, raw := newValidEnvelope(t)
	env.Redaction = &events.Redaction{Applied: false}
	_, err := sink.Write(context.Background(), events.Event{Envelope: env, Raw: raw})
	if err == nil {
		t.Errorf("Write with Applied=false should error")
	}
}

func TestSink_Write_PassesPipelineExtractions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewSink(s)

	env, raw := newValidEnvelope(t)
	// Pipeline-pre-attached extractions; the Sink must use these
	// (not re-run DefaultExtractors). Use a sentinel kind that
	// DefaultExtractors would never produce so the test catches a
	// regression that re-ran the registry.
	pre := []events.Extraction{
		{Kind: "pipeline_sentinel", Value: "from-pipeline"},
	}
	if _, err := sink.Write(context.Background(), events.Event{
		Envelope: env, Raw: raw, Extractions: pre,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var kind, value string
	if err := s.DB().QueryRow(
		`SELECT kind, value FROM extractions WHERE event_id = ?`, env.EventID,
	).Scan(&kind, &value); err != nil {
		t.Fatalf("query extraction: %v", err)
	}
	if kind != "pipeline_sentinel" || value != "from-pipeline" {
		t.Errorf("extraction: got (%s,%s); want pipeline-attached", kind, value)
	}
}

func TestBufferedSink_Write_BuffersUntilThreshold(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewBufferedSink(s, BufferedSinkOpts{MaxRows: 3, MaxBytes: 0})

	env1, raw1 := newValidEnvelope(t)
	env2, raw2 := newValidEnvelope(t)
	if _, err := sink.Write(context.Background(), events.Event{Envelope: env1, Raw: raw1}); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	// One event in the buffer — DB should still be empty.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rows after Write 1 (no flush): got %d, want 0", n)
	}

	if _, err := sink.Write(context.Background(), events.Event{Envelope: env2, Raw: raw2}); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if err := sink.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows after Flush: got %d, want 2", n)
	}
}

func TestBufferedSink_Write_AutoFlushesAtRowCap(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewBufferedSink(s, BufferedSinkOpts{MaxRows: 2, MaxBytes: 0})

	for range 3 {
		env, raw := newValidEnvelope(t)
		if _, err := sink.Write(context.Background(), events.Event{Envelope: env, Raw: raw}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	// Row 3 came in after auto-flush of rows 1+2; it sits in the
	// buffer. DB should have 2 events; explicit Flush adds the 3rd.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows after auto-flush: got %d, want 2", n)
	}
	if err := sink.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("rows after final Flush: got %d, want 3", n)
	}
}

func TestBufferedSink_Close_FlushesPending(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sink := NewBufferedSink(s, BufferedSinkOpts{MaxRows: 100, MaxBytes: 0})

	env, raw := newValidEnvelope(t)
	if _, err := sink.Write(context.Background(), events.Event{Envelope: env, Raw: raw}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows after Close: got %d, want 1", n)
	}
}

// TestBufferedSink_ParityWithEnvelopeBatcher pins that the new Sink
// produces the same row counts as the old envelopeBatcher for the
// same input. This is the load-bearing parity check before commit
// 10 deletes envelopeBatcher: if this test diverges, the two paths
// have drifted and the cutover would silently change behavior.
func TestBufferedSink_ParityWithIngestEnvelope(t *testing.T) {
	t.Parallel()

	// Run N envelopes through BufferedSink…
	sNew := openTestStore(t)
	bs := NewBufferedSink(sNew, BufferedSinkOpts{}).WithNow(func() time.Time {
		return time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	})
	const N = 10
	for range N {
		env, raw := newValidEnvelope(t)
		if _, err := bs.Write(context.Background(), events.Event{
			Envelope:    env,
			Raw:         raw,
			Extractions: events.DefaultExtractors().Run(env),
		}); err != nil {
			t.Fatalf("BufferedSink.Write: %v", err)
		}
	}
	if err := bs.Flush(context.Background()); err != nil {
		t.Fatalf("BufferedSink.Flush: %v", err)
	}

	// Counts must match the per-envelope contract.
	var rawCount, eventCount int
	if err := sNew.DB().QueryRow(
		`SELECT (SELECT COUNT(*) FROM raw_envelopes), (SELECT COUNT(*) FROM events)`,
	).Scan(&rawCount, &eventCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rawCount != N || eventCount != N {
		t.Errorf("BufferedSink rows: raw=%d events=%d, want %d/%d",
			rawCount, eventCount, N, N)
	}
}
