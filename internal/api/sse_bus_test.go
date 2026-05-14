package api

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/wire"
)

// captureHandler is a slog.Handler that appends every emitted
// record to a slice under a mutex. Used by tests to assert that
// the bus produced an expected log line without relying on stderr
// scraping or the test process's global default handler.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

// snapshot returns a copy of the captured records so callers can
// iterate without holding the handler's lock.
func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

func TestSSEBus_PublishToOneSubscriber(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	defer b.Close()

	ch, cancel, ok := b.subscribe()
	if !ok {
		t.Fatal("subscribe rejected on empty bus")
	}
	defer cancel()

	want := wire.StreamEvent{IngestSeq: 1, EventID: "abc", Kind: "user_prompt"}
	b.Publish(want)

	select {
	case got := <-ch:
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for publish")
	}
}

func TestSSEBus_PublishFanOutToManySubscribers(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	defer b.Close()

	const n = 5
	chs := make([]<-chan wire.StreamEvent, 0, n)
	cancels := make([]func(), 0, n)
	for i := range n {
		ch, cancel, ok := b.subscribe()
		if !ok {
			t.Fatalf("subscribe %d rejected", i)
		}
		chs = append(chs, ch)
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	want := wire.StreamEvent{EventID: "fan-out"}
	b.Publish(want)

	for i, ch := range chs {
		select {
		case got := <-ch:
			if got != want {
				t.Errorf("subscriber %d: got %+v", i, got)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: timeout", i)
		}
	}
}

func TestSSEBus_CapAtMaxSubscribers(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	defer b.Close()

	cancels := make([]func(), 0, SSEMaxSubscribers)
	for i := range SSEMaxSubscribers {
		_, cancel, ok := b.subscribe()
		if !ok {
			t.Fatalf("subscribe %d rejected before cap", i)
		}
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	// Cap reached — next subscribe must fail.
	if _, _, ok := b.subscribe(); ok {
		t.Errorf("subscribe at cap+1 should have been rejected")
	}
}

func TestSSEBus_SlowSubscriberDropped(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	defer b.Close()

	ch, cancel, _ := b.subscribe()
	defer cancel()

	// Fill the buffer past capacity — once full, the next
	// publish drops the subscriber. The test cannot guess the
	// exact buffer size, so push enough events to overflow.
	for i := range SSEBufferSize + 10 {
		b.Publish(wire.StreamEvent{IngestSeq: int64(i)})
	}

	// Drain whatever's in the channel; eventually it must close.
	timeout := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed; subscriber correctly dropped
			}
		case <-timeout:
			t.Fatal("slow subscriber was not dropped")
		}
	}
}

func TestSSEBus_SlowSubscriberDropEmitsWarnLog(t *testing.T) {
	t.Parallel()
	rec := &captureHandler{}
	b := newSSEBus(slog.New(rec))
	defer b.Close()

	_, cancel, _ := b.subscribe()
	defer cancel()

	// Overflow the buffer. The exact event that pushes the
	// subscriber over the edge varies (the channel may absorb a
	// couple of extra sends before reporting "would block" — Go's
	// runtime schedules), so push enough that at least one drop
	// is guaranteed.
	for i := range SSEBufferSize * 2 {
		b.Publish(wire.StreamEvent{IngestSeq: int64(i), Kind: "tool_use"})
	}

	var dropRec *slog.Record
	for _, r := range rec.snapshot() {
		if r.Message == "sse: subscriber dropped (slow consumer)" {
			rec := r
			dropRec = &rec
			break
		}
	}
	if dropRec == nil {
		t.Fatal("expected drop warn log, got none")
	}
	if dropRec.Level != slog.LevelWarn {
		t.Errorf("expected Warn level, got %v", dropRec.Level)
	}

	// Required attributes for the log line to be operationally
	// useful (operator filtering on sub_id, knowing which event
	// kind pushed the consumer over the edge).
	want := map[string]bool{
		"sub_id":      false,
		"event_kind":  false,
		"remaining":   false,
		"buffer_size": false,
	}
	dropRec.Attrs(func(a slog.Attr) bool {
		if _, ok := want[a.Key]; ok {
			want[a.Key] = true
		}
		if a.Key == "event_kind" && a.Value.String() != "tool_use" {
			t.Errorf("event_kind: got %q, want %q", a.Value.String(), "tool_use")
		}
		return true
	})
	for k, seen := range want {
		if !seen {
			t.Errorf("drop log missing attr %q", k)
		}
	}
}

func TestSSEBus_CloseDisconnectsSubscribers(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	ch1, _, _ := b.subscribe()
	ch2, _, _ := b.subscribe()

	b.Close()

	for i, ch := range []<-chan wire.StreamEvent{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("subscriber %d: channel still open after Close", i)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: Close did not drain", i)
		}
	}
}

func TestSSEBus_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	b.Close()
	b.Close() // must not panic
}

func TestSSEBus_PublishAfterCloseIsNoOp(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	b.Close()
	// Must not panic; no observable side effects.
	b.Publish(wire.StreamEvent{EventID: "post-close"})
}

// TestSSEBus_SubscribeAfterCloseReturnsFalse pins the
// shutdown-race fix: a request that lands between Close() and
// srv.Shutdown cancelling its r.Context() must NOT register a
// new subscriber. Without this guard the SSE handler would park
// on the channel forever, extending graceful shutdown by the
// full http.Server drain timeout per stranded subscriber.
func TestSSEBus_SubscribeAfterCloseReturnsFalse(t *testing.T) {
	t.Parallel()
	b := newSSEBus(nil)
	b.Close()

	ch, cancel, ok := b.subscribe()
	if ok {
		t.Errorf("subscribe after Close should fail; got ok=true")
	}
	if ch != nil {
		t.Errorf("expected nil channel on closed bus; got %v", ch)
	}
	if cancel != nil {
		t.Errorf("expected nil cancel on closed bus; got non-nil")
	}
}

func TestSSEBus_ConcurrentPublishersAndSubscribers(t *testing.T) {
	t.Parallel()
	// Sanity: many publishers fanning out to many subscribers
	// must not deadlock or lose ordering for fast consumers.
	b := newSSEBus(nil)
	defer b.Close()

	const subscribers = 8
	const publishers = 4
	const eventsPerPublisher = 50

	var wg sync.WaitGroup
	received := make(chan int, subscribers*publishers*eventsPerPublisher)
	cancels := make([]func(), 0, subscribers)

	for i := range subscribers {
		ch, cancel, ok := b.subscribe()
		if !ok {
			t.Fatalf("subscribe %d rejected", i)
		}
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range ch {
				received <- int(ev.IngestSeq)
			}
		}()
	}

	for p := 0; p < publishers; p++ {
		go func(p int) {
			for i := range eventsPerPublisher {
				b.Publish(wire.StreamEvent{IngestSeq: int64(p*eventsPerPublisher + i)})
			}
		}(p)
	}

	// Give publishers + subscribers time to run, then close
	// channels. We only assert no deadlock + no panic.
	time.Sleep(200 * time.Millisecond)
	for _, c := range cancels {
		c()
	}
	wg.Wait()
	close(received)
}
