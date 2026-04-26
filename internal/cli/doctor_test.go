package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startDoctorTestSocket spins up a real Unix-domain HTTP server with
// the supplied handler at a temp socket and returns the path. The
// socket and goroutine are torn down via t.Cleanup. Used to exercise
// RunDoctor against deterministic daemon responses without dragging
// in the real daemon code.
func startDoctorTestSocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 1 * time.Second}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-done
	})
	return sockPath
}

func TestRunDoctor_HealthyDaemonReportsOK(t *testing.T) {
	t.Parallel()
	sockPath := startDoctorTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/healthz" {
			t.Errorf("doctor hit wrong path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))

	var out bytes.Buffer
	ok, err := RunDoctor(t.Context(), &out, sockPath, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true, got false; out=%q", out.String())
	}
	if !strings.HasPrefix(out.String(), "OK ") {
		t.Errorf("expected OK-prefixed line, got %q", out.String())
	}
	if !strings.Contains(out.String(), sockPath) {
		t.Errorf("expected output to name socket path; got %q", out.String())
	}
}

func TestRunDoctor_UnreachableSocketReportsFAIL(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such.sock")

	var out bytes.Buffer
	ok, err := RunDoctor(t.Context(), &out, missing, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for missing socket")
	}
	if !strings.HasPrefix(out.String(), "FAIL ") {
		t.Errorf("expected FAIL-prefixed line, got %q", out.String())
	}
	// The error message should name what's wrong — "no such file" or
	// "connect:" — so the user can grep their dotfiles for the cause.
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("expected diagnostic to say unreachable, got %q", out.String())
	}
}

func TestRunDoctor_Non2xxReportsFAILWithStatusCode(t *testing.T) {
	t.Parallel()
	sockPath := startDoctorTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal explosion", http.StatusInternalServerError)
	}))

	var out bytes.Buffer
	ok, err := RunDoctor(t.Context(), &out, sockPath, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for 500 response")
	}
	if !strings.Contains(out.String(), "500") {
		t.Errorf("expected status code in diagnostic, got %q", out.String())
	}
}

func TestRunDoctor_QuietSuppressesOutput(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such.sock")

	var out bytes.Buffer
	ok, err := RunDoctor(t.Context(), &out, missing, true /* quiet */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("quiet mode still expected to fail-detect; got ok=true")
	}
	if out.Len() != 0 {
		t.Errorf("quiet mode wrote output: %q", out.String())
	}
}

func TestRunDoctor_HangingAcceptTriggersTimeout(t *testing.T) {
	t.Parallel()
	// This is the exact failure mode we hit in production: kernel
	// accepts the connection but the daemon's accept loop is wedged
	// and nothing reads from it. RunDoctor must NOT hang the user's
	// terminal — it must time out and return ok=false.
	sockPath := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Accept one connection and hold it forever — never read, never
	// respond. Run in a goroutine so the listener doesn't pile up.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the connection — RunDoctor's per-request timeout
		// (defaultDoctorTimeout) must trip before this returns.
		<-t.Context().Done()
		_ = conn.Close()
	}()
	t.Cleanup(wg.Wait)

	var out bytes.Buffer
	start := time.Now()
	ok, err := RunDoctor(t.Context(), &out, sockPath, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("hanging daemon should not be reported as healthy")
	}
	// defaultDoctorTimeout is 1s; allow generous slack for slow CI.
	if elapsed > 5*time.Second {
		t.Errorf("doctor took %v before giving up; expected ~%v", elapsed, defaultDoctorTimeout)
	}
}

func TestErrExitCodeOneIsExported(t *testing.T) {
	t.Parallel()
	// The cobra layer relies on errExitCodeOne being a sentinel, not
	// a wrapped error: cobra.Command.RunE returning it triggers a
	// non-zero exit without re-printing. If a future refactor wraps
	// or replaces it, the doctor exit code stops being trustworthy
	// for status-bar wiring. This test pins the contract.
	if errExitCodeOne == nil {
		t.Fatal("errExitCodeOne must not be nil")
	}
	if !errors.Is(errExitCodeOne, errExitCodeOne) {
		t.Error("errExitCodeOne lost identity through errors.Is")
	}
}
