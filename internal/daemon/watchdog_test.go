package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startHealthzServer spins a real Unix-domain HTTP server with the
// supplied /v1/healthz handler and returns the socket path. Both
// the server and the listener are torn down via t.Cleanup.
//
// Used to drive Watchdog tests against deterministic responses
// without dragging in the full daemon.NewServer wiring.
func startHealthzServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/healthz", handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-done
	})
	return sock
}

// silentLogger discards every log line. Used for tests where slog's
// stderr noise would clutter the harness.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeNotify records every state string the watchdog tries to send.
// Returns success unless errOnSend is set. Safe for concurrent use
// because the watchdog goroutine writes while the test reads.
type fakeNotify struct {
	mu        sync.Mutex
	states    []string
	errOnSend error
}

func (f *fakeNotify) call(state string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOnSend != nil {
		return false, f.errOnSend
	}
	f.states = append(f.states, state)
	return true, nil
}

func (f *fakeNotify) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.states))
	copy(out, f.states)
	return out
}

func TestWatchdog_NotifiesOnHealthyProbe(t *testing.T) {
	t.Parallel()
	sock := startHealthzServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	fn := &fakeNotify{}
	w := &Watchdog{
		Interval:     20 * time.Millisecond,
		ProbeTimeout: 200 * time.Millisecond,
		SockPath:     sock,
		Notify:       fn.call,
		Log:          silentLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	got := fn.snapshot()
	if len(got) == 0 {
		t.Fatal("expected at least one WATCHDOG=1 ping, got none")
	}
	for _, s := range got {
		if s != "WATCHDOG=1" {
			t.Errorf("unexpected payload: %q", s)
		}
	}
}

func TestWatchdog_StaysSilentOnNon2xx(t *testing.T) {
	t.Parallel()
	sock := startHealthzServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	})

	fn := &fakeNotify{}
	w := &Watchdog{
		Interval:     20 * time.Millisecond,
		ProbeTimeout: 200 * time.Millisecond,
		SockPath:     sock,
		Notify:       fn.call,
		Log:          silentLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if got := fn.snapshot(); len(got) != 0 {
		t.Errorf("non-2xx must NOT trigger a WATCHDOG=1 ping; got %v", got)
	}
}

func TestWatchdog_StaysSilentOnDialFailure(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such.sock")

	fn := &fakeNotify{}
	w := &Watchdog{
		Interval:     20 * time.Millisecond,
		ProbeTimeout: 200 * time.Millisecond,
		SockPath:     missing,
		Notify:       fn.call,
		Log:          silentLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if got := fn.snapshot(); len(got) != 0 {
		t.Errorf("missing socket must NOT trigger a ping; got %v", got)
	}
}

// TestWatchdog_StaysSilentOnHangingAccept is the load-bearing test:
// the production bug was a daemon whose accept loop was wedged but
// whose listening socket was still LISTEN per the kernel. A naive
// heartbeat would have gone "I'm alive!" forever; an in-process
// roundtrip times out. We assert the watchdog notices.
func TestWatchdog_StaysSilentOnHangingAccept(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection — never read, never write.
			// This is exactly the symptom the production
			// daemon got stuck in.
			go func(conn net.Conn) {
				<-stop
				_ = conn.Close()
			}(c)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = ln.Close()
		wg.Wait()
	})

	fn := &fakeNotify{}
	w := &Watchdog{
		Interval:     50 * time.Millisecond,
		ProbeTimeout: 100 * time.Millisecond,
		SockPath:     sock,
		Notify:       fn.call,
		Log:          silentLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if got := fn.snapshot(); len(got) != 0 {
		t.Errorf("hanging accept must keep the watchdog silent so systemd's "+
			"deadline trips; got %d pings: %v", len(got), got)
	}
}

func TestWatchdog_HonorsContextCancellation(t *testing.T) {
	t.Parallel()
	sock := startHealthzServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	fn := &fakeNotify{}
	w := &Watchdog{
		Interval:     20 * time.Millisecond,
		ProbeTimeout: 100 * time.Millisecond,
		SockPath:     sock,
		Notify:       fn.call,
		Log:          silentLogger(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Let it run, then cancel and assert prompt return.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return within 500ms after ctx cancel")
	}
}

// TestWatchdog_FatalNotifyExitsRun pins the design choice: if
// SdNotify returns an error (NOTIFY_SOCKET unreachable, malformed
// payload), the goroutine exits rather than spinning forever. A
// living goroutine that can't notify is the worst-of-both — we
// burn CPU AND systemd's deadline still trips.
func TestWatchdog_FatalNotifyExitsRun(t *testing.T) {
	t.Parallel()
	sock := startHealthzServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	fn := &fakeNotify{errOnSend: errors.New("boom: NOTIFY_SOCKET unreachable")}
	w := &Watchdog{
		Interval:     20 * time.Millisecond,
		ProbeTimeout: 100 * time.Millisecond,
		SockPath:     sock,
		Notify:       fn.call,
		Log:          silentLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// ok — goroutine exited because the first probe
		// succeeded, the first Notify failed fatally, and Run
		// returned WITHOUT waiting for ctx.
	case <-ctx.Done():
		t.Error("Run did not exit on Notify error; only ctx fired")
	}
}

func TestWatchdog_RecoversFromProbePanic(t *testing.T) {
	t.Parallel()
	// A Notify that panics simulates a bug deep inside the ping
	// path. The defer/recover in Run must turn it into a logged
	// exit, not a crash that takes the whole daemon with it.
	sock := startHealthzServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var calls atomic.Int32
	w := &Watchdog{
		Interval:     20 * time.Millisecond,
		ProbeTimeout: 100 * time.Millisecond,
		SockPath:     sock,
		Notify: func(state string) (bool, error) {
			calls.Add(1)
			panic("watchdog: simulated bug")
		},
		Log: silentLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		// If Run didn't recover, this would crash the goroutine
		// AND the test process. The fact that the test reaches
		// `close(done)` is the assertion.
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return after a panicking Notify; recover missing?")
	}
	if calls.Load() == 0 {
		t.Error("expected Notify to be invoked at least once before the panic")
	}
}

func TestStart_NoOpWhenWatchdogDisabled(t *testing.T) {
	// t.Setenv forbids t.Parallel; the env mutation is process-wide.
	// SdWatchdogEnabled keys off WATCHDOG_USEC and WATCHDOG_PID;
	// in a normal test env neither is set, so Start should
	// silently return nil and no goroutine starts. The fact
	// that this test doesn't hang or leak goroutines IS the
	// assertion.
	t.Setenv("WATCHDOG_USEC", "")
	t.Setenv("WATCHDOG_PID", "")
	if err := Start(t.Context(), "/nonexistent.sock", silentLogger()); err != nil {
		t.Errorf("Start with no env should return nil, got %v", err)
	}
}
