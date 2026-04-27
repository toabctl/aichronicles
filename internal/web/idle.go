package web

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// idleTracker watches http.Server.ConnState callbacks and fires
// onIdle once the connection count has been zero for `timeout`
// continuous time. Pairs with socket-activated deployment (the
// systemd socket unit stays listening, so the next request after
// shutdown spins the service back up).
//
// Counts only StateNew and StateClosed/StateHijacked transitions —
// Active/Idle within a keepalive connection don't change the count.
// SSE handlers stay in StateActive for the life of the stream;
// they never trigger StateIdle/StateClosed until the client (or
// the server itself) disconnects, so a tab actively rendering the
// livefeed naturally pins the count above zero.
type idleTracker struct {
	timeout time.Duration
	onIdle  func() // invoked once when the timer fires

	mu     sync.Mutex
	active int
	timer  *time.Timer
	fired  bool // onIdle is one-shot; protect against re-entry
}

// newIdleTracker returns a tracker that fires onIdle after `timeout`
// of zero connections. timeout ≤ 0 disables tracking entirely:
// the returned tracker counts but never fires, useful when callers
// want a single code path regardless of whether idle-shutdown is on.
func newIdleTracker(timeout time.Duration, onIdle func()) *idleTracker {
	return &idleTracker{timeout: timeout, onIdle: onIdle}
}

// trackConn is the http.Server.ConnState hook. Cheap, holds a
// short-lived mutex; safe for the per-connection callback.
func (t *idleTracker) trackConn(_ net.Conn, state http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch state {
	case http.StateNew:
		t.active++
		t.cancelTimerLocked()
	case http.StateClosed, http.StateHijacked:
		if t.active > 0 {
			t.active--
		}
		if t.active == 0 {
			t.startTimerLocked()
		}
	}
	// http.StateActive and http.StateIdle stay within the lifetime
	// of an existing connection — no count change.
}

// startTimerLocked arms the idle-fire timer. Must be called with
// t.mu held. Idempotent if a timer is already running (not
// expected since transitions are serialised by ConnState).
func (t *idleTracker) startTimerLocked() {
	if t.timeout <= 0 || t.fired {
		return
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(t.timeout, t.fire)
}

// cancelTimerLocked drops a pending idle timer. Must be called
// with t.mu held.
func (t *idleTracker) cancelTimerLocked() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// fire is the timer callback. Re-checks the count under the lock
// in case a connection arrived between the timer firing and us
// running — common race when the timeout is short.
func (t *idleTracker) fire() {
	t.mu.Lock()
	if t.active > 0 || t.fired {
		t.mu.Unlock()
		return
	}
	t.fired = true
	t.mu.Unlock()

	// Call onIdle outside the lock so the callback can take its
	// own state without risking a re-entrant trackConn deadlock.
	if t.onIdle != nil {
		t.onIdle()
	}
}

// stop releases the timer (if any). Safe to call multiple times.
// Used in test cleanup; production lifecycle is bounded by the
// service exiting.
func (t *idleTracker) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelTimerLocked()
}

// idleShutdownContext returns a context that cancels when the
// tracker fires. Lets Run treat idle-shutdown identically to
// SIGTERM/ctx.Done() — same drain path, same shutdown timeout.
func idleShutdownContext(parent context.Context, t *idleTracker) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	if t == nil || t.timeout <= 0 {
		return ctx, cancel
	}
	t.onIdle = cancel
	return ctx, func() {
		t.stop()
		cancel()
	}
}
