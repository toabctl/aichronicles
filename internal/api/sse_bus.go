package api

import (
	"io"
	"log/slog"
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
	// nextID stamps each new subscriber with a monotonic id so
	// drop logs (and any future per-subscriber metrics) can refer
	// to a specific connection without exposing the *sseSubscriber
	// pointer. Wraps after 2^64 publishes, which is never.
	nextID atomic.Uint64
	// log emits operational events — subscriber drops on slow-
	// consumer overflow are the headline use case. Always non-nil
	// after newSSEBus (nil input falls back to a discard handler)
	// so callers don't need a per-site guard.
	log *slog.Logger
}

// sseSubscriber is one connected SSE consumer's slot on the bus.
// ch is bounded; if a publish would block, the subscriber is
// dropped. id is the bus-assigned monotonic identifier, surfaced
// in slog drop records so an operator can correlate the disconnect
// with the matching subscribe log line on the handler side.
type sseSubscriber struct {
	id       uint64
	ch       chan wire.StreamEvent
	overflow atomic.Bool
}

// newSSEBus builds an empty bus. A nil log is replaced with a
// discard handler so Publish never needs a nil-check; callers that
// want drops to actually surface (production wiring, tests
// capturing records) must pass a real logger.
func newSSEBus(log *slog.Logger) *sseBus {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &sseBus{
		subs: make(map[*sseSubscriber]struct{}),
		log:  log,
	}
}

// subscribe registers a new subscriber and returns its channel
// plus a cancel func. cancel must be called by the consumer
// (typically via defer) so the bus releases the slot.
//
// Returns (nil, nil, false) when the bus is at SSEMaxSubscribers
// OR after Close() has flipped the closed flag. Caller maps either
// to HTTP 429 / 503 (the SSE handler chooses 429 today). Without
// the closed check, a request arriving in the small window between
// Close() and srv.Shutdown cancelling its r.Context() could
// successfully subscribe and then sit forever on a channel Publish
// will never write to — graceful shutdown drains for the full
// http.Server timeout per stranded subscriber.
func (b *sseBus) subscribe() (<-chan wire.StreamEvent, func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return nil, nil, false
	}
	if len(b.subs) >= SSEMaxSubscribers {
		return nil, nil, false
	}
	s := &sseSubscriber{
		id: b.nextID.Add(1),
		ch: make(chan wire.StreamEvent, SSEBufferSize),
	}
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
// Drops emit a slog.Warn record with the event kind that pushed
// the subscriber over and the post-drop subscriber count, so an
// operator looking at a "lost event" in the live feed can trace
// it back to a specific disconnected subscriber (the same id is
// emitted by the SSE handler on its subscribe / disconnect log
// lines). The log call happens AFTER releasing b.mu so a slow
// slog handler (journal writer, network sink) doesn't extend the
// writer's lock hold for every other publisher. Drops are rare,
// but the I/O cost of slog is unbounded.
//
// No-op after Close.
func (b *sseBus) Publish(ev wire.StreamEvent) {
	if b.closed.Load() {
		return
	}
	// dropped collects the sub_ids removed in this Publish so we
	// can log them after releasing b.mu. Pre-allocate one slot —
	// the common case is zero drops; the rare case is one.
	type dropRec struct{ id uint64 }
	var dropped []dropRec
	remaining := 0

	b.mu.Lock()
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			// Buffer full → drop the subscriber.
			s.overflow.Store(true)
			delete(b.subs, s)
			close(s.ch)
			dropped = append(dropped, dropRec{id: s.id})
		}
	}
	remaining = len(b.subs)
	b.mu.Unlock()

	for _, d := range dropped {
		b.log.Warn("sse: subscriber dropped (slow consumer)",
			"sub_id", d.id,
			"event_kind", ev.Kind,
			"remaining", remaining,
			"buffer_size", SSEBufferSize)
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
