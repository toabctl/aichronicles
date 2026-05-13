package cli

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestRefuseIfDaemonRunning_NoSocket verifies the safe-to-proceed
// path: when the socket file is absent, the daemon is not running
// and the helper returns nil so the maintenance subcommand owns the
// store exclusively.
func TestRefuseIfDaemonRunning_NoSocket(t *testing.T) {
	t.Setenv("AICHRONICLES_API_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	if err := RefuseIfDaemonRunning(t.Context(), ""); err != nil {
		t.Fatalf("RefuseIfDaemonRunning with missing socket: got %v, want nil", err)
	}
}

// TestRefuseIfDaemonRunning_HealthyDaemon verifies the refuse path:
// a daemon answering 200 to /v1/healthz triggers ErrDaemonRunning.
func TestRefuseIfDaemonRunning_HealthyDaemon(t *testing.T) {

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // test-only short-lived listener
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv("AICHRONICLES_API_SOCKET", sockPath)
	err = RefuseIfDaemonRunning(t.Context(), "")
	if !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("RefuseIfDaemonRunning with healthy daemon: got %v, want ErrDaemonRunning", err)
	}
}

// TestRefuseIfDaemonRunning_DeadSocketFile verifies that a stale
// socket file (file exists, no listener) is treated as "daemon
// down" — apiclient surfaces ErrSocketUnavailable when the dial
// fails, and the helper passes through nil.
func TestRefuseIfDaemonRunning_DeadSocketFile(t *testing.T) {

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stale.sock")
	// Create a regular file at the socket path so it exists on
	// disk but won't accept connections. The dial will error with
	// ErrSocketUnavailable.
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket file: %v", err)
	}

	t.Setenv("AICHRONICLES_API_SOCKET", sockPath)
	if err := RefuseIfDaemonRunning(t.Context(), ""); err != nil {
		t.Fatalf("RefuseIfDaemonRunning with stale socket: got %v, want nil", err)
	}
}

// TestRefuseIfDaemonRunning_UnhealthyDaemon documents the
// conservative refuse-on-error policy: a reachable daemon that
// returns 500 still holds the SQLite write lock, so the helper
// refuses. Operators must stop the daemon first regardless of
// whether healthz is currently happy.
func TestRefuseIfDaemonRunning_UnhealthyDaemon(t *testing.T) {

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "unhealthy.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{ //nolint:gosec // test-only short-lived listener
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv("AICHRONICLES_API_SOCKET", sockPath)
	err = RefuseIfDaemonRunning(t.Context(), "")
	if !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("RefuseIfDaemonRunning with 500 daemon: got %v, want ErrDaemonRunning", err)
	}
}
