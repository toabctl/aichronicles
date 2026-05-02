package events

import (
	"context"
	"errors"
	"iter"
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
}

func (s *fakeSink) Write(_ context.Context, e Event) (Result, error) {
	if s.writeErr != nil {
		return Result{}, s.writeErr
	}
	s.written = append(s.written, e)
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

func redactedEnv(id string) *Envelope {
	return &Envelope{
		EventID:   id,
		Redaction: &Redaction{Applied: true},
	}
}

func TestPipeline_Process_RejectsUnredactedWhenRequired(t *testing.T) {
	t.Parallel()
	p := Pipeline{Sink: &fakeSink{}, RequireRedaction: true}
	_, err := p.Process(context.Background(), Event{Envelope: &Envelope{}})
	if !errors.Is(err, ErrRedactionRequired) {
		t.Errorf("got %v, want ErrRedactionRequired", err)
	}
}

func TestPipeline_Process_AllowsUnredactedWhenNotRequired(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	p := Pipeline{Sink: sink, RequireRedaction: false}
	if _, err := p.Process(context.Background(), Event{Envelope: &Envelope{EventID: "abc"}}); err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if len(sink.written) != 1 {
		t.Errorf("Sink.Write call count: got %d, want 1", len(sink.written))
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
	p := Pipeline{Sink: sink, Extractors: registry, RequireRedaction: true}
	if _, err := p.Process(context.Background(), Event{Envelope: redactedEnv("abc")}); err != nil {
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
	p := Pipeline{Sink: sink, Extractors: registry, RequireRedaction: true}
	pre := []Extraction{{Kind: "from_source", Value: "preset"}}
	_, err := p.Process(context.Background(), Event{
		Envelope:    redactedEnv("abc"),
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
		{Envelope: redactedEnv("a")},
		{Envelope: redactedEnv("b")},
		{Envelope: redactedEnv("c")},
	}}
	p := Pipeline{Sink: sink, RequireRedaction: true}
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
	// Middle event is unredacted → Process returns ErrRedactionRequired.
	// Pipeline must log + count, then continue.
	src := &staticSource{events: []Event{
		{Envelope: redactedEnv("a")},
		{Envelope: &Envelope{EventID: "bad"}}, // missing Redaction
		{Envelope: redactedEnv("c")},
	}}
	p := Pipeline{Sink: sink, RequireRedaction: true}
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
			{Envelope: redactedEnv("a")},
			{}, // ignored when err != nil
			{Envelope: redactedEnv("c")},
		},
		errs: []error{nil, errors.New("source bork"), nil},
	}
	p := Pipeline{Sink: sink, RequireRedaction: true}
	stats, _ := p.Run(context.Background(), src)
	if stats.Processed != 2 || stats.Errors != 1 {
		t.Errorf("stats=%+v, want Processed=2 Errors=1", stats)
	}
}

func TestPipeline_Run_FlushErrorPropagates(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{flushErr: errors.New("disk full")}
	src := &staticSource{events: []Event{{Envelope: redactedEnv("a")}}}
	p := Pipeline{Sink: sink, RequireRedaction: true}
	_, err := p.Run(context.Background(), src)
	if err == nil || !errors.Is(err, sink.flushErr) {
		t.Errorf("expected wrapped flush error, got %v", err)
	}
}

func TestPipeline_Run_CtxCanceledHaltsImmediately(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	src := &staticSource{events: []Event{
		{Envelope: redactedEnv("a")},
		{Envelope: redactedEnv("b")},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	p := Pipeline{Sink: sink, RequireRedaction: true}
	_, err := p.Run(ctx, src)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
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
