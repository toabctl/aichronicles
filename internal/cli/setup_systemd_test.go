package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingRunner captures every systemctl invocation so tests can
// assert on the command sequence without shelling out.
type recordingRunner struct {
	calls [][]string
	err   error
}

func (r *recordingRunner) run(args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.err
}

func TestInstallSystemdUnits_WritesBothUnitsIntoEmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &recordingRunner{}

	report, err := InstallSystemdUnits(dir, r.run)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	for _, name := range unitFilenames {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("unit %s not written: %v", name, err)
			continue
		}
		want := unitContent(name)
		if !bytes.Equal(data, want) {
			t.Errorf("unit %s: content mismatch\n-got %q\n+want %q", name, data, want)
		}
	}

	if !strings.Contains(report, dir) {
		t.Errorf("report should mention unit dir: %q", report)
	}
	if !strings.Contains(report, "aichronicles-api.socket") {
		t.Errorf("report should mention the socket unit: %q", report)
	}
}

func TestInstallSystemdUnits_RunsDaemonReloadAndEnable(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	if _, err := InstallSystemdUnits(t.TempDir(), r.run); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 systemctl invocations, got %d: %v", len(r.calls), r.calls)
	}
	if got := strings.Join(r.calls[0], " "); got != "daemon-reload" {
		t.Errorf("first call: got %q, want daemon-reload", got)
	}
	wantEnable := "enable --now aichronicles-api.socket aichronicles-web.socket"
	if got := strings.Join(r.calls[1], " "); got != wantEnable {
		t.Errorf("second call: got %q, want %q", got, wantEnable)
	}
}

func TestInstallSystemdUnits_IsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &recordingRunner{}

	report1, err := InstallSystemdUnits(dir, r.run)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if strings.Contains(report1, "no changes") {
		t.Errorf("first install should report writes, got %q", report1)
	}

	firstMtime := fileMtime(t, filepath.Join(dir, "aichronicles-api.socket"))

	report2, err := InstallSystemdUnits(dir, r.run)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !strings.Contains(report2, "no changes") {
		t.Errorf("second install should be no-op, got %q", report2)
	}

	// Touch-time unchanged — we did not rewrite identical content.
	if fileMtime(t, filepath.Join(dir, "aichronicles-api.socket")) != firstMtime {
		t.Errorf("identical content should not cause a rewrite")
	}
}

func TestInstallSystemdUnits_OverwritesOutdatedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "aichronicles-api.socket")
	if err := os.WriteFile(path, []byte("# stale from a prior version\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := InstallSystemdUnits(dir, (&recordingRunner{}).run); err != nil {
		t.Fatalf("install: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, systemdSocketUnit) {
		t.Errorf("stale socket unit should be overwritten with embedded content")
	}
}

func TestInstallSystemdUnits_PropagatesRunnerError(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{err: errMockSystemctl}
	_, err := InstallSystemdUnits(t.TempDir(), r.run)
	if err == nil {
		t.Fatal("expected error when runner fails")
	}
}

func TestInstallSystemdUnits_CreatesDirWhenMissing(t *testing.T) {
	t.Parallel()
	nested := filepath.Join(t.TempDir(), "does", "not", "exist")
	if _, err := InstallSystemdUnits(nested, (&recordingRunner{}).run); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestDefaultSystemdUserDir_UsesXDGConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-example")
	got, err := defaultSystemdUserDir()
	if err != nil {
		t.Fatalf("defaultSystemdUserDir: %v", err)
	}
	want := "/tmp/xdg-example/systemd/user"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDefaultSystemdUserDir_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := defaultSystemdUserDir()
	if err != nil {
		t.Fatalf("defaultSystemdUserDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "systemd", "user")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEmbeddedWebServiceOrdersAfterAPI guards a load-bearing systemd
// directive: aichronicles-web.service MUST declare
// After=aichronicles-api.service so a fresh boot (or `make install`
// bouncing both) doesn't activate web against an un-migrated SQLite
// schema. Regression-protects the embedded asset.
func TestEmbeddedWebServiceOrdersAfterAPI(t *testing.T) {
	t.Parallel()
	if !bytes.Contains(systemdWebServiceUnit, []byte("After=aichronicles-api.service")) {
		t.Fatalf("aichronicles-web.service must declare After=aichronicles-api.service; got:\n%s",
			systemdWebServiceUnit)
	}
}

// fileMtime is a tiny helper for idempotency checks.
func fileMtime(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.ModTime().UnixNano()
}

// errMockSystemctl is a sentinel returned by recordingRunner in tests
// that intentionally make the runner fail.
var errMockSystemctl = &mockErr{msg: "mock systemctl failure"}

type mockErr struct{ msg string }

func (e *mockErr) Error() string { return e.msg }
