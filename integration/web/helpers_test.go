//go:build e2e

// Package web hosts end-to-end tests that drive a real headless
// Chrome against a real aichronicles web server backed by a real
// SQLite store. Unit tests verify the markup the server emits;
// these tests verify the markup actually does what we want once a
// browser parses it — htmx swaps fire, SSE drives DOM updates, CSS
// classes resolve.
//
// Build tag is `e2e` (not `integration`) so the existing
// integration suite — and the pre-commit hook that runs it —
// doesn't suddenly require Chrome. Run with:
//
//	go test -tags=e2e ./integration/web/...
//
// Tests skip when no Chrome/Chromium binary is found, so a fresh
// CI image without a browser still passes the build.
package web_e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/web"
)

// chromeBinaries lists the executables we look for on PATH. First
// match wins. Order: distro chromium first (smaller), then Chrome.
var chromeBinaries = []string{
	"chromium",
	"chromium-browser",
	"google-chrome",
	"google-chrome-stable",
	"chrome",
}

// requireChrome returns the path to a working browser binary, or
// skips the test if none is available. Avoids hard-failing on
// CI images without Chrome installed.
func requireChrome(t *testing.T) string {
	t.Helper()
	for _, name := range chromeBinaries {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skipf("no Chrome/Chromium binary on PATH; install one of %v to run e2e tests",
		chromeBinaries)
	return ""
}

// testEnv bundles everything one e2e test needs: an opened store
// and a running web server bound to an ephemeral port. Stop()
// shuts both down. All resources tied to t — no manual cleanup
// required as long as you call Stop() (defer it).
type testEnv struct {
	Store   *store.Store
	BaseURL string

	stop    func()
	stopped bool
	stopMu  sync.Mutex
}

// startEnv brings up a fresh store and web server on an ephemeral
// loopback port. The store lives in t.TempDir() so each test gets
// its own DB. Returns a *testEnv whose Stop must be deferred.
func startEnv(t *testing.T) *testEnv {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "aichronicles.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = st.Close()
		t.Fatalf("listen: %v", err)
	}

	srv := web.NewServer(st, web.Config{Listener: ln, ShutdownTimeout: 2 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Run(runCtx)
		close(done)
	}()

	env := &testEnv{
		Store:   st,
		BaseURL: "http://" + ln.Addr().String(),
	}
	env.stop = func() {
		env.stopMu.Lock()
		defer env.stopMu.Unlock()
		if env.stopped {
			return
		}
		env.stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("server did not shut down within 5s after cancel")
		}
		_ = st.Close()
	}
	t.Cleanup(env.stop)

	// Hit /healthz to confirm the server is actually accepting
	// connections before the test starts driving it. Avoids the
	// race where chromedp connects faster than http.Server.Serve
	// has begun accepting.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(env.BaseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up at %s: %v", env.BaseURL, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return env
}

// Stop shuts down the server and closes the store. Idempotent —
// safe to call from multiple deferred slots.
func (e *testEnv) Stop() { e.stop() }

// ingestEvent writes one envelope to the store via the normal
// ingest path so all triggers fire (sessions aggregate, FTS index,
// etc.). Returns the derived session_id so callers can look up the
// row in the rendered page.
func (e *testEnv) ingestEvent(t *testing.T, kind, sourceSession, content string, ts time.Time) string {
	t.Helper()
	role := "user"
	switch kind {
	case "session_end", "session_start":
		role = "system"
	case "tool_use", "tool_failure":
		role = "tool"
	case "assistant_message":
		role = "assistant"
	}
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            kind,
		Role:            role,
		TsSource:        ts.UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     content,
		Payload:         map[string]any{},
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, err := e.Store.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.IngestEnvelope(t.Context(), tx, env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest %s: %v", kind, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return events.DeriveSessionID("claude-code", sourceSession)
}

// newBrowser returns a chromedp context backed by a headless Chrome
// process. Cancel function shuts both the browser and any child
// pages. Pages forked from this context inherit the same browser
// instance, so consider creating sub-contexts with chromedp.NewContext
// for separate pages within one test.
func newBrowser(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	binary := requireChrome(t)

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(binary),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.DisableGPU,
		// Sandboxing trips on minimal CI images; localhost-only
		// content makes this safe.
		chromedp.NoSandbox,
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(t.Context(), allocOpts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	// Touch the browser so allocator failures surface here, not
	// halfway through a test's first action.
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		t.Fatalf("launch browser: %v", err)
	}

	cancel := func() {
		browserCancel()
		allocCancel()
	}
	t.Cleanup(cancel)
	return browserCtx, cancel
}

// pollUntil polls fn() at 50ms intervals until it returns nil or
// the deadline expires. Used by tests to wait for SSE-driven DOM
// updates without arbitrary sleeps. Returns the last fn error on
// timeout so the test message names the actual mismatch.
func pollUntil(t *testing.T, deadline time.Duration, fn func() error) {
	t.Helper()
	until := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(until) {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("polling deadline exceeded with no error captured")
	}
	t.Fatalf("pollUntil: %v", lastErr)
}

// withTimeout shadows context.WithTimeout for chromedp ops so each
// browser action gets a sensible per-step deadline without making
// the test caller verbose.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
