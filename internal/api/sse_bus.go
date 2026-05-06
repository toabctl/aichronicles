package api

import (
	"sync"
	"sync/atomic"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SSEMaxSubscribers caps simultaneous SSE subscribers system-wide.
// Each subscriber spends one Go goroutine + one buffered channel.
// 32 is plenty for a personal-use tool and keeps the resource
// cost bounded if a runaway client (or a forgotten browser tab)
// tries to open dozens.
const SSEMaxSubscribers = 32

// SSEBufferSize is the per-subscriber bounded buffer. A slow
// consumer that fills its buffer is dropped (the next publish
// closes its channel) so it never blocks publishers or other
// subscribers. 64 is wide enough that a normal consumer gets at
// least a couple of seconds of breathing room during a bursty
// ingest before being declared slow.
const SSEBufferSize = 64

// sseBus is a tiny in-process publish/subscribe channel for live
// activity. The api process owns ingest writes, so it is the
// authoritative source of "a new event was stored" — Publish is
// called on the same goroutine that committed the write,
// immediately after the Sink returns.
//
// Versus the legacy "poll PRAGMA data_version every 500ms"
// approach: in-process pub/sub has zero polling cost when idle,
// no missed events between polls, and produces no SQLite traffic.
//
// Concurrency: safe for many publishers and subscribers in
// parallel; one mutex protects the subscriber map.
type sseBus struct {
	mu     sync.Mutex
	subs   map[*sseSubscriber]struct{}
	closed atomic.Bool
}

// sseSubscriber is one connected SSE consumer's slot on the bus.
// ch is bounded; if a publish would block, the subscriber is
// dropped.
type sseSubscriber struct {
	ch       chan wire.StreamEvent
	overflow atomic.Bool
}

// newSSEBus builds an empty bus.
func newSSEBus() *sseBus {
	return &sseBus{subs: make(map[*sseSubscriber]struct{})}
}

// subscribe registers a new subscriber and returns its channel
// plus a cancel func. cancel must be called by the consumer
// (typically via defer) so the bus releases the slot.
//
// If the bus is at SSEMaxSubscribers, returns (nil, nil, false).
// Caller maps that to HTTP 429.
func (b *sseBus) subscribe() (<-chan wire.StreamEvent, func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs) >= SSEMaxSubscribers {
		return nil, nil, false
	}
	s := &sseSubscriber{ch: make(chan wire.StreamEvent, SSEBufferSize)}
	b.subs[s] = struct{}{}
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s]; ok {
			delete(b.subs, s)
			close(s.ch)
		}
	}
	return s.ch, cancel, true
}

// Publish fans an event out to every live subscriber. A subscriber
// whose buffer is full is marked overflowed and gets disconnected
// — the next read on the channel returns the zero value. This
// prevents one slow client from backing up the publisher's
// goroutine.
//
// No-op after Close.
func (b *sseBus) Publish(ev wire.StreamEvent) {
	if b.closed.Load() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			// Buffer full → drop the subscriber.
			s.overflow.Store(true)
			delete(b.subs, s)
			close(s.ch)
		}
	}
}

// Close terminates every active subscriber. Used at server
// shutdown so the SSE handler goroutines exit cleanly.
func (b *sseBus) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
}
