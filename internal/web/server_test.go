package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// openTempStore opens a fresh aichronicles store at a per-test
// temp path. Caller doesn't have to close it — t.Cleanup handles
// that. Mirrors the helper in internal/store/store_test.go to
// keep this package independent.
func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// startTestServer wires NewServer onto a Listener bound to an
// ephemeral 127.0.0.1 port, runs it in a goroutine, and returns
// the base URL plus a cleanup func that triggers shutdown via
// ctx cancel. Used by every route-level test.
func startTestServer(t *testing.T, st *store.Store) (baseURL string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := Config{Listener: ln, ShutdownTimeout: time.Second}
	s := NewServer(st, cfg, nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(done)
	}()

	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not shut down within timeout")
		}
	}
	return "http://" + ln.Addr().String(), stop
}

func TestHealthz_ReturnsOK(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("body: got %q, want ok", string(body))
	}
}

func TestHealthz_RejectsPOST(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	resp, err := http.Post(base+"/healthz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Go 1.22 ServeMux returns 405 for method-mismatch on a
	// pattern restricted to GET — encodes the intent that this is
	// a read-only endpoint.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405 (read-only endpoint)", resp.StatusCode)
	}
}

func TestUnknownRoute_404(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	resp, err := http.Get(base + "/this-does-not-exist")
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestRun_GracefulShutdownOnContextCancel asserts the server
// returns nil from Run when ctx is cancelled (rather than
// returning the http.ErrServerClosed underlying signal).
func TestRun_GracefulShutdownOnContextCancel(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := NewServer(st, Config{Listener: ln, ShutdownTimeout: time.Second}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// Give Serve a moment to spin up before we tear it down.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned %v on graceful shutdown, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s of ctx cancel")
	}
}

func TestIsPublicBind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		bind string
		want bool
	}{
		{"", false},
		{"127.0.0.1", false},
		{"127.0.0.5", false},
		{"::1", false},
		{"localhost", false},
		{"0.0.0.0", true},
		{"::", true},
		{"192.168.1.10", true},
		{"example.internal", true},
	}
	for _, tc := range cases {
		if got := isPublicBind(tc.bind); got != tc.want {
			t.Errorf("isPublicBind(%q) = %v, want %v", tc.bind, got, tc.want)
		}
	}
}

func TestNewServer_DefaultsApplied(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	s := NewServer(st, Config{}, nil)
	if s.cfg.Bind != DefaultBind {
		t.Errorf("Bind: got %q, want %q", s.cfg.Bind, DefaultBind)
	}
	if s.cfg.Port != DefaultPort {
		t.Errorf("Port: got %d, want %d", s.cfg.Port, DefaultPort)
	}
	if s.cfg.ShutdownTimeout == 0 {
		t.Errorf("ShutdownTimeout: zero, want non-zero default")
	}
}
