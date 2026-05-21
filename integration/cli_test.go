//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// isolateEnv scopes XDG + notification env so RunHook never reads
// the user's real config or fires real DBus notifications from a test.
func isolateEnv(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AICHRONICLES_DISABLE_NOTIFY", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "runtime"))
}

// startAPIDaemon spins up an api.Server on sock with its ingest
// worker running in the background. Returns a wait func that
// blocks until ingest_pending is empty (or a 2s deadline).
// Cleanup (server shutdown + worker stop) is registered via
// t.Cleanup so callers don't need to defer anything themselves.
//
// Used by integration tests that exercise the full hook → UDS →
// worker → SQLite path. The wait func is the test's barrier
// between RunHook (which only enqueues) and the assertion against
// downstream tables.
func startAPIDaemon(t *testing.T, s *store.Store, sock string) func() {
	t.Helper()
	srv := api.NewServer(s, nil)

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	go func() { _ = srv.Worker().Run(workerCtx) }()

	shutdown, err := api.ListenAndServe(sock, srv.Handler(), slog.New(slog.DiscardHandler))
	if err != nil {
		cancelWorker()
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		cancelWorker()
		<-srv.Worker().Done()
	})

	return func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			n, err := store.CountPending(t.Context(), s.DB())
			if err != nil {
				t.Fatalf("CountPending: %v", err)
			}
			if n == 0 {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("worker did not drain ingest_pending within 2s")
	}
}

func TestCLI_IngestRoundTrip(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.OpenMigrate(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitDrain := startAPIDaemon(t, s, sock)

	hook := []byte(`{
		"session_id": "e2e-1",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/tmp/e2e",
		"prompt": "what is the time"
	}`)

	var stderr bytes.Buffer
	if err := cli.RunHook(t.Context(), bytes.NewReader(hook), &stderr, sock, ""); err != nil {
		t.Fatalf("RunHook returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	waitDrain()

	// The event should now be in the store.
	wantSessionID := events.DeriveSessionID("claude-code", "e2e-1")
	var kind, cwd, content string
	err = s.DB().QueryRow(
		`SELECT kind, cwd, content_text FROM events WHERE session_id=?`, wantSessionID,
	).Scan(&kind, &cwd, &content)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if kind != "user_prompt" {
		t.Errorf("kind: got %q", kind)
	}
	if cwd != "/tmp/e2e" {
		t.Errorf("cwd: got %q", cwd)
	}
	if content != "what is the time" {
		t.Errorf("content: got %q", content)
	}

	// FTS should find it too.
	var ftsHits int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH ?`, "time",
	).Scan(&ftsHits)
	if ftsHits != 1 {
		t.Errorf("FTS: got %d, want 1", ftsHits)
	}
}

func TestCLI_IngestRedactsSecretsEndToEnd(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.OpenMigrate(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitDrain := startAPIDaemon(t, s, sock)

	// User accidentally pasted an Anthropic API key into a prompt.
	secret := "sk-ant-" + strings.Repeat("a", 40)
	hook := []byte(`{
		"session_id": "e2e-secret",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/tmp/e2e",
		"prompt": "my key is ` + secret + ` do not log"
	}`)

	var stderr bytes.Buffer
	if err := cli.RunHook(t.Context(), bytes.NewReader(hook), &stderr, sock, ""); err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	waitDrain()

	wantSessionID := events.DeriveSessionID("claude-code", "e2e-secret")
	var content string
	err = s.DB().QueryRow(
		`SELECT content_text FROM events WHERE session_id=?`, wantSessionID,
	).Scan(&content)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if strings.Contains(content, "sk-ant-") {
		t.Fatalf("stored content still contains raw secret: %q", content)
	}
	if !strings.Contains(content, "<redacted:anthropic_api_key>") {
		t.Errorf("expected redacted marker: %q", content)
	}

	// The raw envelope must also have been scrubbed before it hit the
	// daemon — otherwise a rogue admin with DB access could recover the
	// secret from raw_envelopes.json_bytes.
	var raw string
	err = s.DB().QueryRow(
		`SELECT envelope_json FROM raw_envelopes WHERE event_id IN
			(SELECT event_id FROM events WHERE session_id=?)`, wantSessionID,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("query raw_envelopes: %v", err)
	}
	if strings.Contains(raw, "sk-ant-") {
		t.Fatalf("raw envelope still contains secret: %q", raw)
	}
	if !strings.Contains(raw, `"applied":true`) {
		t.Errorf("expected redaction.applied=true in raw envelope: %q", raw)
	}
}

func TestCLI_IngestRespectsDenyPaths(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.OpenMigrate(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	waitDrain := startAPIDaemon(t, s, sock)

	// Write a config that denies /work/nda.
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "aichronicles")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.toml")
	cfgBody := "[capture]\ndeny_paths = [\"/work/nda\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	// Hook event from inside /work/nda — must be dropped silently.
	hook := []byte(`{
		"session_id": "denied",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/work/nda/secret",
		"prompt": "confidential"
	}`)

	var stderr bytes.Buffer
	if err := cli.RunHook(t.Context(), bytes.NewReader(hook), &stderr, sock, ""); err != nil {
		t.Fatalf("RunIngest: %v", err)
	}

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&n)
	if n != 0 {
		t.Fatalf("denied envelope was persisted: %d rows", n)
	}
	if !strings.Contains(stderr.String(), "capture.deny_paths") {
		t.Errorf("expected deny-paths log line in stderr: %q", stderr.String())
	}

	// A control hook from an allowed cwd still lands.
	hookOK := []byte(`{
		"session_id": "allowed",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/work/public",
		"prompt": "ok"
	}`)
	stderr.Reset()
	if err := cli.RunHook(t.Context(), bytes.NewReader(hookOK), &stderr, sock, ""); err != nil {
		t.Fatalf("RunHook allowed: %v", err)
	}
	waitDrain()
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM raw_envelopes`).Scan(&n)
	if n != 1 {
		t.Fatalf("allowed envelope should land: got %d rows", n)
	}
}

// TestCLI_IngestUnreachableDaemon_LogsAndContinues pins the design
// contract: when the daemon is unreachable, the ingest CLI must
// (a) never return an error to the calling hook (would break Claude
// Code's prompt loop) and (b) emit a structured log line so the
// failure is at least visible in stderr / journal.
//
// Renamed from `_DoesNotFail` because the old name implied "no
// failure handling needed" — but the hidden silent-failure path is
// exactly what let a 30-hour outage drop ~hundreds of events
// without the user noticing. The companion test below pins the
// outage-flag side effect that catches the next such outage early.
func TestCLI_IngestUnreachableDaemon_LogsAndContinues(t *testing.T) {
	isolateEnv(t)
	hook := []byte(`{"session_id":"x","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)
	var stderr bytes.Buffer

	err := cli.RunHook(t.Context(), bytes.NewReader(hook), &stderr, filepath.Join(t.TempDir(), "nope.sock"), "")
	if err != nil {
		t.Fatalf("expected nil error even when daemon unreachable, got %v", err)
	}
	if !strings.Contains(stderr.String(), "aichronicles hook") {
		t.Errorf("expected stderr to mention hook error, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "event_dropped") {
		t.Errorf("expected stderr to log the dropped event (msg=event_dropped), got %q", stderr.String())
	}
}

// TestCLI_IngestUnreachableDaemon_FlipsOutageFlag pins the user-
// visibility side of the contract. Silent-on-the-hook is fine; what
// is NOT fine is silent on the user. After a failed POST, the
// outage flag must exist on disk — that's what the rate-limited
// notifier checks before sending a desktop banner. If a future
// refactor removes the maybeNotifyOutage call from RunIngest, this
// test fails immediately instead of letting another 30-hour outage
// drop events with no signal to the user.
func TestCLI_IngestUnreachableDaemon_FlipsOutageFlag(t *testing.T) {
	isolateEnv(t)
	hook := []byte(`{"session_id":"x","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)

	err := cli.RunHook(t.Context(), bytes.NewReader(hook), &bytes.Buffer{},
		filepath.Join(t.TempDir(), "nope.sock"), "")
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}

	// XDG_RUNTIME_DIR was redirected by isolateEnv; the flag must
	// land at <isolated>/aichronicles/outage.flag.
	flag := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "aichronicles", "outage.flag")
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("expected outage flag at %s, got: %v", flag, err)
	}
}

// TestCLI_IngestRecoveryClearsOutageFlag covers the other half of
// the lifecycle: once the daemon is reachable again, the next
// successful POST must drop the flag so a future outage triggers a
// fresh notification rather than being suppressed by stale state.
func TestCLI_IngestRecoveryClearsOutageFlag(t *testing.T) {
	isolateEnv(t)

	// First: fail once to plant the flag.
	missingSock := filepath.Join(t.TempDir(), "nope.sock")
	hook := []byte(`{"session_id":"x","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)
	if err := cli.RunHook(t.Context(), bytes.NewReader(hook), &bytes.Buffer{}, missingSock, ""); err != nil {
		t.Fatalf("first RunIngest: %v", err)
	}
	flag := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "aichronicles", "outage.flag")
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("flag should exist after first failure: %v", err)
	}

	// Now: bring up a real daemon and ingest successfully.
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	s, err := store.OpenMigrate(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	startAPIDaemon(t, s, sock)

	if err := cli.RunHook(t.Context(), bytes.NewReader(hook), &bytes.Buffer{}, sock, ""); err != nil {
		t.Fatalf("second RunIngest: %v", err)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("flag should be cleared after a successful post; stat err = %v", err)
	}
}

// TestCLI_IngestHangingDaemon_FlipsOutageFlag reproduces the EXACT
// production failure mode that caused 30 hours of silent event drops:
// the daemon process is alive, the kernel reports the UDS as LISTEN,
// connect() succeeds — but accept() never returns / the daemon never
// reads the request body. From the hook's point of view the POST
// hangs and times out. The fix being pinned: the timeout must trip,
// the CLI must NOT propagate an error to the hook (Claude's prompt
// loop would break), AND the outage flag must be written so the
// rate-limited notifier fires a desktop banner.
//
// Without this test, a regression that (e.g.) widened the ingest
// timeout to "infinite" or stopped calling maybeNotifyOutage on
// timeout-class errors would silently re-introduce the bug.
func TestCLI_IngestHangingDaemon_FlipsOutageFlag(t *testing.T) {
	isolateEnv(t)

	// A unix-domain listener that accepts but never reads from or
	// responds to its connections. Mirrors the production state
	// where the daemon's accept loop is wedged.
	sockPath := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection until the test finishes — never
			// read, never write. The CLI must time out and treat
			// this as a failure.
			go func(c net.Conn) {
				<-stop
				_ = c.Close()
			}(conn)
		}
	}()
	// Cleanup order matters: signal the per-conn holders, then
	// CLOSE THE LISTENER (unblocks Accept), THEN wait for the
	// accept goroutine. Closing in the other order deadlocks.
	defer func() {
		close(stop)
		_ = ln.Close()
		wg.Wait()
	}()

	hook := []byte(`{"session_id":"x","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)
	var stderr bytes.Buffer

	start := time.Now()
	err = cli.RunHook(t.Context(), bytes.NewReader(hook), &stderr, sockPath, "")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunHook must not surface the timeout to the hook, got: %v", err)
	}
	// defaultHookTimeout is 5s; allow generous slack for slow CI.
	if elapsed > 10*time.Second {
		t.Errorf("RunHook blocked for %v on a hanging daemon; the timeout did not trip", elapsed)
	}
	if !strings.Contains(stderr.String(), "event_dropped") {
		t.Errorf("expected stderr to log the dropped event, got %q", stderr.String())
	}

	// The crucial assertion: a hanging-daemon failure must trip the
	// outage flag. This is the side effect the rate-limited desktop
	// notifier polls — without it, the user gets zero signal that
	// events are being dropped, exactly as happened in production.
	flag := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "aichronicles", "outage.flag")
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("hanging-daemon ingest must flip the outage flag at %s; got: %v",
			flag, err)
	}
}

// TestCLI_HookHonorsParentContextCancellation pins the Ctrl-C
// path. Pre-fix, RunHook derived its per-request deadline from
// context.Background(), so cobra's signal-installed root context
// (cmd.Context()) had no path to the in-flight POST — a user
// interrupting `aichronicles hook` waited out the full
// IngestTimeout. The fix routes the parent context through;
// cancelling it must unblock the call promptly.
func TestCLI_HookHonorsParentContextCancellation(t *testing.T) {
	isolateEnv(t)

	// Same hanging-daemon listener as
	// TestCLI_IngestHangingDaemon_FlipsOutageFlag: accept the
	// connection, then never read or respond. With Background()
	// as parent the call would wait the full defaultHookTimeout
	// (5s); with the parent ctx canceled, it should exit fast.
	sockPath := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				<-stop
				_ = c.Close()
			}(conn)
		}
	}()
	defer func() {
		close(stop)
		_ = ln.Close()
		wg.Wait()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel after a short delay so the call has time to start
	// the POST and park on the unresponsive socket.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	hook := []byte(`{"session_id":"x","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)
	var stderr bytes.Buffer
	start := time.Now()
	if err := cli.RunHook(ctx, bytes.NewReader(hook), &stderr, sockPath, ""); err != nil {
		t.Fatalf("RunHook must not surface cancellation to the hook, got: %v", err)
	}
	elapsed := time.Since(start)

	// Without the fix the call would wait out the full
	// IngestTimeout (≥2s default). The cancellation should unblock
	// it well below that — pin at 1s as the regression bound.
	if elapsed > time.Second {
		t.Errorf("RunHook blocked for %v after parent ctx cancel; signal didn't propagate", elapsed)
	}
}

// TestCLI_Ingest503QueueFull_FlipsOutageFlag pins a subtle bit of
// the visibility contract: when the daemon is up and accepting
// connections, but its ingest_pending queue is saturated (return
// 503), the hook must still trip the outage flag. The
// daemon-up-but-saturated case is functionally the same to the
// user as daemon-down — events are being silently dropped — and
// before this branch existed the outage counter / recovery banner
// never fired for a queue-full condition. Regression test for
// arch_review_2026_05_13 HIGH #2.
func TestCLI_Ingest503QueueFull_FlipsOutageFlag(t *testing.T) {
	isolateEnv(t)
	sockPath := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv503 := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"title":"Ingest queue is full","status":503,"detail":"pending=10000 max=10000"}`))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv503.Serve(ln) }()
	defer func() { _ = srv503.Close() }()

	hook := []byte(`{"session_id":"qf","hook_event_name":"UserPromptSubmit","cwd":"/","prompt":"hi"}`)
	var stderr bytes.Buffer
	if err := cli.RunHook(t.Context(), bytes.NewReader(hook), &stderr, sockPath, ""); err != nil {
		t.Fatalf("RunHook must not surface 503 to the hook, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "event_dropped") {
		t.Errorf("expected stderr to log the dropped event, got %q", stderr.String())
	}
	flag := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "aichronicles", "outage.flag")
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("503-queue-full must flip the outage flag at %s; got: %v",
			flag, err)
	}
}
