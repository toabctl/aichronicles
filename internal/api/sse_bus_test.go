package api

import (
	"sync"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/pkg/api"
)

func TestSSEBus_PublishToOneSubscriber(t *testing.T) {
	t.Parallel()
	b := newSSEBus()
	defer b.Close()

	ch, cancel, ok := b.subscribe()
	if !ok {
		t.Fatal("subscribe rejected on empty bus")
	}
	defer cancel()

	want := api.StreamEvent{IngestSeq: 1, EventID: "abc", Kind: "user_prompt"}
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
	b := newSSEBus()
	defer b.Close()

	const n = 5
	chs := make([]<-chan api.StreamEvent, 0, n)
	cancels := make([]func(), 0, n)
	for i := 0; i < n; i++ {
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

	want := api.StreamEvent{EventID: "fan-out"}
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
	b := newSSEBus()
	defer b.Close()

	cancels := make([]func(), 0, SSEMaxSubscribers)
	for i := 0; i < SSEMaxSubscribers; i++ {
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
	b := newSSEBus()
	defer b.Close()

	ch, cancel, _ := b.subscribe()
	defer cancel()

	// Fill the buffer past capacity — once full, the next
	// publish drops the subscriber. The test cannot guess the
	// exact buffer size, so push enough events to overflow.
	for i := 0; i < SSEBufferSize+10; i++ {
		b.Publish(api.StreamEvent{IngestSeq: int64(i)})
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

func TestSSEBus_CloseDisconnectsSubscribers(t *testing.T) {
	t.Parallel()
	b := newSSEBus()
	ch1, _, _ := b.subscribe()
	ch2, _, _ := b.subscribe()

	b.Close()

	for i, ch := range []<-chan api.StreamEvent{ch1, ch2} {
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
	b := newSSEBus()
	b.Close()
	b.Close() // must not panic
}

func TestSSEBus_PublishAfterCloseIsNoOp(t *testing.T) {
	t.Parallel()
	b := newSSEBus()
	b.Close()
	// Must not panic; no observable side effects.
	b.Publish(api.StreamEvent{EventID: "post-close"})
}

func TestSSEBus_ConcurrentPublishersAndSubscribers(t *testing.T) {
	t.Parallel()
	// Sanity: many publishers fanning out to many subscribers
	// must not deadlock or lose ordering for fast consumers.
	b := newSSEBus()
	defer b.Close()

	const subscribers = 8
	const publishers = 4
	const eventsPerPublisher = 50

	var wg sync.WaitGroup
	received := make(chan int, subscribers*publishers*eventsPerPublisher)
	cancels := make([]func(), 0, subscribers)

	for i := 0; i < subscribers; i++ {
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
			for i := 0; i < eventsPerPublisher; i++ {
				b.Publish(api.StreamEvent{IngestSeq: int64(p*eventsPerPublisher + i)})
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
