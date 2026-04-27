package web

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleTracker_FiresAfterNoConnections(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	tr := newIdleTracker(20*time.Millisecond, func() { fired.Add(1) })
	defer tr.stop()

	// Open then close one connection. The close should arm the
	// timer; with no further opens it must fire within ~timeout.
	tr.trackConn(nil, http.StateNew)
	tr.trackConn(nil, http.StateClosed)

	deadline := time.Now().Add(500 * time.Millisecond)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Errorf("idle handler did not fire (got %d times)", fired.Load())
	}
}

func TestIdleTracker_DoesNotFireWhileConnectionsOpen(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	tr := newIdleTracker(20*time.Millisecond, func() { fired.Add(1) })
	defer tr.stop()

	tr.trackConn(nil, http.StateNew)
	// No close; sleep beyond the timeout to confirm fire stays at 0.
	time.Sleep(60 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("idle handler fired with one connection still open")
	}
}

func TestIdleTracker_NewConnectionCancelsPendingFire(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	tr := newIdleTracker(50*time.Millisecond, func() { fired.Add(1) })
	defer tr.stop()

	tr.trackConn(nil, http.StateNew)
	tr.trackConn(nil, http.StateClosed) // arms timer
	time.Sleep(10 * time.Millisecond)
	tr.trackConn(nil, http.StateNew) // cancels timer
	time.Sleep(80 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("timer should have been cancelled by new conn (fired=%d)", fired.Load())
	}
}

func TestIdleTracker_FiresOnceOnly(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	tr := newIdleTracker(15*time.Millisecond, func() { fired.Add(1) })
	defer tr.stop()

	tr.trackConn(nil, http.StateNew)
	tr.trackConn(nil, http.StateClosed)
	time.Sleep(50 * time.Millisecond)

	// Re-trigger by opening + closing more conns. The one-shot
	// guarantee should hold even after additional cycles.
	tr.trackConn(nil, http.StateNew)
	tr.trackConn(nil, http.StateClosed)
	time.Sleep(50 * time.Millisecond)

	if fired.Load() != 1 {
		t.Errorf("expected single fire, got %d", fired.Load())
	}
}

func TestIdleTracker_ZeroTimeoutDisablesTracking(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	tr := newIdleTracker(0, func() { fired.Add(1) })
	defer tr.stop()

	tr.trackConn(nil, http.StateNew)
	tr.trackConn(nil, http.StateClosed)
	time.Sleep(40 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("zero timeout should never fire")
	}
}

func TestIdleTracker_HandlesNegativeCounts(t *testing.T) {
	t.Parallel()
	// A spurious StateClosed without a matching StateNew shouldn't
	// drive the counter negative; defensive guard pinned here.
	tr := newIdleTracker(0, nil)
	tr.trackConn(nil, http.StateClosed)
	if tr.active != 0 {
		t.Errorf("active count should clamp at 0, got %d", tr.active)
	}
}

func TestIdleShutdownContext_CancelsOnFire(t *testing.T) {
	t.Parallel()
	tr := newIdleTracker(20*time.Millisecond, nil)
	ctx, cleanup := idleShutdownContext(context.Background(), tr)
	defer cleanup()

	tr.trackConn(nil, http.StateNew)
	tr.trackConn(nil, http.StateClosed)
	select {
	case <-ctx.Done():
		// success
	case <-time.After(500 * time.Millisecond):
		t.Errorf("idle ctx did not cancel after timer fired")
	}
}

func TestIdleShutdownContext_NoTimeoutPassesThroughParent(t *testing.T) {
	t.Parallel()
	tr := newIdleTracker(0, nil)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, cleanup := idleShutdownContext(parent, tr)
	defer cleanup()

	if ctx.Err() != nil {
		t.Errorf("ctx should not be done yet")
	}
	cancelParent()
	select {
	case <-ctx.Done():
		// success
	case <-time.After(50 * time.Millisecond):
		t.Errorf("ctx should propagate parent cancellation")
	}
}
