package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
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
	var sawHealthz bool
	sockPath := startDoctorTestSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// doctor also makes advisory pipeline-health reads
		// (/v1/llm-outputs, /v1/sessions). Those must not affect the
		// health verdict, so answer them emptily rather than
		// asserting they don't happen — the probe under test here is
		// the healthz roundtrip.
		switch r.URL.Path {
		case "/v1/healthz":
			sawHealthz = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/llm-outputs":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"outputs":[]}`))
		case "/v1/sessions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		default:
			t.Errorf("doctor hit unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	var out bytes.Buffer
	ok, err := RunDoctor(t.Context(), &out, sockPath, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true, got false; out=%q", out.String())
	}
	if !sawHealthz {
		t.Error("doctor never probed /v1/healthz")
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

// TestReportPipelineStaleness covers the check added after the
// analysis pipeline died silently twice.
//
// Both times capture kept working — events flowed, the daemon probe
// said healthy, the web UI listed new sessions — while nothing was
// being summarised. The second outage ran 34 days and was found by
// accident. The daemon is the data plane; every failure so far has
// been in the control plane it knows nothing about, so a healthy
// daemon is not a healthy system.
func TestReportPipelineStaleness(t *testing.T) {
	t.Parallel()

	t.Run("stale artifact warns and names the age", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		c := apiForStore(t, s)
		// 34 days old — the real outage.
		seedLLMOutputAt(t, s, time.Now().Add(-34*24*time.Hour))

		var b bytes.Buffer
		reportPipelineStaleness(context.Background(), &b, c)
		out := b.String()
		if !strings.Contains(out, "WARN") {
			t.Errorf("expected a WARN for a 34-day-old artifact, got %q", out)
		}
		if !strings.Contains(out, "34 days old") {
			t.Errorf("warning should name the age so it's actionable, got %q", out)
		}
	})

	t.Run("recent artifact stays quiet", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		c := apiForStore(t, s)
		seedLLMOutputAt(t, s, time.Now().Add(-2*time.Hour))

		var b bytes.Buffer
		reportPipelineStaleness(context.Background(), &b, c)
		if b.Len() != 0 {
			t.Errorf("a 2-hour-old artifact is healthy; got %q", b.String())
		}
	})

	t.Run("just inside the threshold stays quiet", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		c := apiForStore(t, s)
		seedLLMOutputAt(t, s, time.Now().Add(-pipelineStaleAfter+time.Hour))

		var b bytes.Buffer
		reportPipelineStaleness(context.Background(), &b, c)
		if b.Len() != 0 {
			t.Errorf("inside the threshold must not warn; got %q", b.String())
		}
	})

	// An empty store is a fresh install, not a stall. Warning here
	// would train the reader to ignore the line — which is exactly how
	// a real warning gets missed later.
	t.Run("empty store stays quiet", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		c := apiForStore(t, s)

		var b bytes.Buffer
		reportPipelineStaleness(context.Background(), &b, c)
		if b.Len() != 0 {
			t.Errorf("a fresh install must not warn; got %q", b.String())
		}
	})

	// Sessions captured but nothing ever analysed — the shape of a
	// recovery where `setup systemd` ran and `setup cron` did not.
	t.Run("sessions but no artifacts warns", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		c := apiForStore(t, s)
		ingestForDoctor(t, s)

		var b bytes.Buffer
		reportPipelineStaleness(context.Background(), &b, c)
		out := b.String()
		if !strings.Contains(out, "WARN") || !strings.Contains(out, "never run") {
			t.Errorf("expected a never-run warning, got %q", out)
		}
	})
}

// TestReportCronTimers is the check that would have caught the actual
// outage. Recovery after a reinstall needs two commands — `setup
// systemd` for the daemon and `setup cron` for the timers — and only
// the first has visible consequences.
// Not parallel: uses t.Setenv, which Go forbids alongside t.Parallel.
func TestReportCronTimers(t *testing.T) {
	t.Run("no timers installed warns", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		if err := os.MkdirAll(filepath.Join(dir, "systemd", "user"), 0o700); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		reportCronTimers(&b)
		if !strings.Contains(b.String(), "setup cron") {
			t.Errorf("warning should name the fix, got %q", b.String())
		}
	})

	t.Run("timers installed stays quiet", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		unitDir := filepath.Join(dir, "systemd", "user")
		if err := os.MkdirAll(unitDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(unitDir, "aichronicles-cron-induction.timer"),
			[]byte("[Timer]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		reportCronTimers(&b)
		if b.Len() != 0 {
			t.Errorf("installed timers must not warn; got %q", b.String())
		}
	})
}

// seedLLMOutputAt writes one summary row stamped at ts.
func seedLLMOutputAt(t *testing.T, s *store.Store, ts time.Time) {
	t.Helper()
	if err := store.WithTx(context.Background(), s.DB(), func(tx *sql.Tx) error {
		_, _, err := store.SaveLLMOutput(context.Background(), tx, &store.LLMOutput{
			Kind:        store.LLMKindSummary,
			Model:       "test-model",
			PromptHash:  "doctor-" + ts.Format("20060102150405.000"),
			Body:        `{"topic":"t"}`,
			CreatedAtMs: ts.UnixMilli(),
		})
		return err
	}); err != nil {
		t.Fatalf("seed llm output: %v", err)
	}
}

// ingestForDoctor puts one real session in the store so the
// "captured but never analysed" branch has something to see.
func ingestForDoctor(t *testing.T, s *store.Store) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-doctor",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		ContentText:     "hello",
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(context.Background(), s.DB(), func(tx *sql.Tx) error {
		_, _, err := store.IngestEnvelopeWithExtractions(
			context.Background(), tx, env, raw, time.Now().UnixMilli(), nil)
		return err
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}
