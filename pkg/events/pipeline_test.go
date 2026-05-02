package events

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
)

// fakeSink records writes and replays canned errors. Lets pipeline
// tests assert what the Sink was called with without standing up
// a real SQLite store.
type fakeSink struct {
	written  []Event
	flushed  int
	closed   int
	writeErr error
	flushErr error
	stats    SinkStats
}

func (s *fakeSink) Write(_ context.Context, e Event) (Result, error) {
	if s.writeErr != nil {
		return Result{}, s.writeErr
	}
	s.written = append(s.written, e)
	s.stats.Imported++
	id := ""
	if e.Envelope != nil {
		id = e.Envelope.EventID
	}
	return Result{EventID: id}, nil
}

func (s *fakeSink) Flush(_ context.Context) error {
	s.flushed++
	return s.flushErr
}

func (s *fakeSink) Close() error {
	s.closed++
	return nil
}

func (s *fakeSink) Stats() SinkStats { return s.stats }

// staticSource yields a fixed slice of (Event, err) pairs and then
// stops. Used to drive Pipeline.Run without filesystem or HTTP I/O.
type staticSource struct {
	events []Event
	errs   []error
}

func (s *staticSource) Events(_ context.Context) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for i, e := range s.events {
			var err error
			if i < len(s.errs) {
				err = s.errs[i]
			}
			if !yield(e, err) {
				return
			}
		}
	}
}

// noopRedactor is the standard Redactor for pipeline tests that don't
// care about redaction content — it just sets Applied=true so Pipeline
// invariants are satisfied. NewScannerRedactor(nil) gives the same
// behavior; we use the helper for readability at call sites.
func noopRedactor() Redactor { return NewScannerRedactor(nil) }

func envWithID(id string) *Envelope { return &Envelope{EventID: id} }

func TestPipeline_Process_NilRedactorReturnsErr(t *testing.T) {
	t.Parallel()
	// A Pipeline without a Redactor is a programmer error in
	// production — fail loud. Tests that genuinely want a no-op
	// pass NewScannerRedactor(nil) instead.
	sink := &fakeSink{}
	p := Pipeline{Sink: sink}
	_, err := p.Process(context.Background(), Event{Envelope: envWithID("abc")})
	if !errors.Is(err, ErrRedactionRequired) {
		t.Errorf("got %v, want ErrRedactionRequired", err)
	}
	if len(sink.written) != 0 {
		t.Errorf("Sink must not be called when Redactor is nil; got %d writes", len(sink.written))
	}
}

func TestPipeline_Process_RunsExtractorsWhenEmpty(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	registry := &ExtractorRegistry{
		Content: []Extractor{func(_ *Envelope) []Extraction {
			return []Extraction{{Kind: "fake", Value: "x"}}
		}},
	}
	p := Pipeline{Sink: sink, Extractors: registry, Redactor: noopRedactor()}
	if _, err := p.Process(context.Background(), Event{Envelope: envWithID("abc")}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(sink.written) != 1 || len(sink.written[0].Extractions) != 1 {
		t.Errorf("Sink saw %v; expected 1 event with 1 extraction", sink.written)
	}
}

func TestPipeline_Process_PreservesPreAttachedExtractions(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	// Registry would emit "from_registry"; the pre-attached set
	// is "from_source". The pre-attached one wins because the
	// pipeline sees it as already-populated and skips dispatch.
	registry := &ExtractorRegistry{
		Content: []Extractor{func(_ *Envelope) []Extraction {
			return []Extraction{{Kind: "from_registry"}}
		}},
	}
	p := Pipeline{Sink: sink, Extractors: registry, Redactor: noopRedactor()}
	pre := []Extraction{{Kind: "from_source", Value: "preset"}}
	_, err := p.Process(context.Background(), Event{
		Envelope:    envWithID("abc"),
		Extractions: pre,
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	got := sink.written[0].Extractions
	if len(got) != 1 || got[0].Kind != "from_source" {
		t.Errorf("expected pre-attached extractions kept, got %v", got)
	}
}

func TestPipeline_Run_AccumulatesStatsAcrossEvents(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	src := &staticSource{events: []Event{
		{Envelope: envWithID("a")},
		{Envelope: envWithID("b")},
		{Envelope: envWithID("c")},
	}}
	p := Pipeline{Sink: sink, Redactor: noopRedactor()}
	stats, err := p.Run(context.Background(), src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Processed != 3 || stats.Errors != 0 || stats.Deduped != 0 {
		t.Errorf("stats=%+v, want Processed=3 Errors=0 Deduped=0", stats)
	}
	if sink.flushed != 1 {
		t.Errorf("Flush called %d times, want 1", sink.flushed)
	}
}

func TestPipeline_Run_PerEventErrorsCounted_NotAborted(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	// Middle event has a nil envelope → Process returns "nil
	// envelope" error. Pipeline must log + count, then continue.
	src := &staticSource{events: []Event{
		{Envelope: envWithID("a")},
		{Envelope: nil},
		{Envelope: envWithID("c")},
	}}
	p := Pipeline{Sink: sink, Redactor: noopRedactor()}
	stats, err := p.Run(context.Background(), src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Processed != 2 || stats.Errors != 1 {
		t.Errorf("stats=%+v, want Processed=2 Errors=1", stats)
	}
}

func TestPipeline_Run_SourceErrorCountedThenContinues(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	src := &staticSource{
		events: []Event{
			{Envelope: envWithID("a")},
			{}, // ignored when err != nil
			{Envelope: envWithID("c")},
		},
		errs: []error{nil, errors.New("source bork"), nil},
	}
	p := Pipeline{Sink: sink, Redactor: noopRedactor()}
	stats, _ := p.Run(context.Background(), src)
	if stats.Processed != 2 || stats.Errors != 1 {
		t.Errorf("stats=%+v, want Processed=2 Errors=1", stats)
	}
}

func TestPipeline_Run_FlushErrorPropagates(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{flushErr: errors.New("disk full")}
	src := &staticSource{events: []Event{{Envelope: envWithID("a")}}}
	p := Pipeline{Sink: sink, Redactor: noopRedactor()}
	_, err := p.Run(context.Background(), src)
	if err == nil || !errors.Is(err, sink.flushErr) {
		t.Errorf("expected wrapped flush error, got %v", err)
	}
}

func TestPipeline_Run_CtxCanceledHaltsImmediately(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	src := &staticSource{events: []Event{
		{Envelope: envWithID("a")},
		{Envelope: envWithID("b")},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	p := Pipeline{Sink: sink, Redactor: noopRedactor()}
	_, err := p.Run(ctx, src)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// fakeRedactor records calls and prepends a tag to ContentText so
// tests can assert "redactor ran before Sink.Write." Apply is
// idempotent: it skips the rewrite once env.Redaction.Applied=true
// is set, mirroring the contract a real scanner-backed redactor
// satisfies via stable replacement tokens.
type fakeRedactor struct {
	calls int
	tag   string
}

func (r *fakeRedactor) Apply(env *Envelope) {
	r.calls++
	if env == nil {
		return
	}
	if env.Redaction != nil && env.Redaction.Applied {
		return
	}
	if env.ContentText != "" {
		env.ContentText = r.tag + ":" + env.ContentText
	}
	env.Redaction = &Redaction{Applied: true}
}

func TestPipeline_Process_RunsRedactorBeforeSink(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	red := &fakeRedactor{tag: "REDACTED"}
	p := Pipeline{Sink: sink, Redactor: red}
	env := &Envelope{EventID: "abc", ContentText: "hello"}
	if _, err := p.Process(context.Background(), Event{Envelope: env}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if red.calls != 1 {
		t.Errorf("Redactor.Apply calls: got %d, want 1", red.calls)
	}
	if len(sink.written) != 1 {
		t.Fatalf("Sink wrote %d, want 1", len(sink.written))
	}
	if got := sink.written[0].Envelope.ContentText; got != "REDACTED:hello" {
		t.Errorf("Sink saw content %q; expected redactor to have run before Sink.Write", got)
	}
	if sink.written[0].Envelope.Redaction == nil || !sink.written[0].Envelope.Redaction.Applied {
		t.Errorf("Applied flag not set on envelope reaching Sink: %+v", sink.written[0].Envelope.Redaction)
	}
}

func TestPipeline_Process_RedactorSetsAppliedFlag(t *testing.T) {
	t.Parallel()
	// Server-side redaction is the single point of enforcement;
	// the Redactor sets Redaction.Applied=true on the envelope
	// reaching the Sink, regardless of what the client claimed.
	sink := &fakeSink{}
	red := &fakeRedactor{tag: "REDACTED"}
	p := Pipeline{Sink: sink, Redactor: red}
	env := &Envelope{EventID: "abc"}
	if _, err := p.Process(context.Background(), Event{Envelope: env}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(sink.written) != 1 {
		t.Fatalf("expected one Sink.Write; got %d", len(sink.written))
	}
	got := sink.written[0].Envelope.Redaction
	if got == nil || !got.Applied {
		t.Errorf("Sink saw envelope without Applied=true: %+v", got)
	}
}

func TestPipeline_Process_RedactorRemarshalsRawBytes(t *testing.T) {
	t.Parallel()
	// Pipeline must replace Event.Raw with the re-marshaled
	// post-redaction envelope. Storing the original POST body
	// would re-introduce the very secret the redactor just
	// scrubbed. The Sink sees the redacted bytes.
	sink := &fakeSink{}
	red := &fakeRedactor{tag: "REDACTED"}
	p := Pipeline{Sink: sink, Redactor: red}
	original := []byte(`{"event_id":"abc","content_text":"hello"}`)
	env := &Envelope{EventID: "abc", ContentText: "hello"}

	if _, err := p.Process(context.Background(), Event{Envelope: env, Raw: original}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(sink.written) != 1 {
		t.Fatalf("Sink wrote %d events", len(sink.written))
	}
	got := string(sink.written[0].Raw)
	if got == string(original) {
		t.Errorf("Sink saw the unredacted POST body; expected re-marshaled redacted bytes. got=%s", got)
	}
	if !strings.Contains(got, "REDACTED:hello") {
		t.Errorf("re-marshaled Raw does not contain redacted content: %s", got)
	}
}

func TestPipeline_Process_RedactorIdempotentAcrossDoubleProcess(t *testing.T) {
	t.Parallel()
	// Running the Pipeline twice on the same envelope (e.g. a
	// Source applied edge redaction and the Pipeline applies it
	// again) yields stable output. Required for the migration
	// window where both layers run.
	sink := &fakeSink{}
	red := &fakeRedactor{tag: "REDACTED"}
	p := Pipeline{Sink: sink, Redactor: red}
	env := &Envelope{EventID: "abc", ContentText: "hello"}

	if _, err := p.Process(context.Background(), Event{Envelope: env}); err != nil {
		t.Fatalf("first Process: %v", err)
	}
	first := env.ContentText
	if first != "REDACTED:hello" {
		t.Fatalf("first pass content %q; want REDACTED:hello", first)
	}

	// Re-process the same (now-redacted) envelope. Redactor must
	// see Applied=true and short-circuit.
	if _, err := p.Process(context.Background(), Event{Envelope: env}); err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if env.ContentText != first {
		t.Errorf("second pass mutated content: %q != %q", env.ContentText, first)
	}
	if red.calls != 2 {
		t.Errorf("Apply was called %d times; want 2 (idempotent, but still entered)", red.calls)
	}
}

func TestPipeline_Process_NilEnvelopeIsRejected(t *testing.T) {
	t.Parallel()
	// Defensive: nil envelope must not reach the redactor or the
	// Sink. Same guard before and after the redactor change.
	sink := &fakeSink{}
	red := &fakeRedactor{tag: "REDACTED"}
	p := Pipeline{Sink: sink, Redactor: red}
	_, err := p.Process(context.Background(), Event{Envelope: nil})
	if err == nil {
		t.Fatal("expected error for nil envelope")
	}
	if red.calls != 0 {
		t.Errorf("Redactor.Apply must not be called for nil envelope; got %d calls", red.calls)
	}
	if len(sink.written) != 0 {
		t.Errorf("Sink must not be called for nil envelope; got %d writes", len(sink.written))
	}
}

func TestScannerRedactor_NilScannerStillSetsAppliedFlag(t *testing.T) {
	t.Parallel()
	r := NewScannerRedactor(nil)
	env := &Envelope{ContentText: "any text"}
	r.Apply(env)
	if env.Redaction == nil || !env.Redaction.Applied {
		t.Errorf("Applied flag not set: %+v", env.Redaction)
	}
	if env.ContentText != "any text" {
		t.Errorf("nil scanner shouldn't mutate content: got %q", env.ContentText)
	}
}
