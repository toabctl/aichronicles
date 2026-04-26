package notify

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNoop_SendNeverErrors(t *testing.T) {
	t.Parallel()
	if err := Noop().Send("title", "body"); err != nil {
		t.Errorf("Noop.Send returned error: %v", err)
	}
}

func TestNew_DisabledReturnsNoop(t *testing.T) {
	t.Parallel()
	n := New(false)
	if err := n.Send("t", "b"); err != nil {
		t.Errorf("disabled notifier should noop, got %v", err)
	}
}

func TestNew_EnvVarForcesNoop(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv(disableEnvVar, "1")
	n := New(true)
	if err := n.Send("t", "b"); err != nil {
		t.Errorf("env-var disabled should noop, got %v", err)
	}
}

func TestOutageTracker_FreshStateAllowsNotify(t *testing.T) {
	t.Parallel()
	tr := NewOutageTracker(filepath.Join(t.TempDir(), "outage.flag"))
	if !tr.ShouldNotify() {
		t.Error("fresh tracker should allow notification")
	}
}

func TestOutageTracker_MarkThenShouldNotifyFalse(t *testing.T) {
	t.Parallel()
	tr := NewOutageTracker(filepath.Join(t.TempDir(), "outage.flag"))
	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}
	if tr.ShouldNotify() {
		t.Error("after MarkNotified, ShouldNotify should be false")
	}
}

func TestOutageTracker_ClearReopensWindow(t *testing.T) {
	t.Parallel()
	tr := NewOutageTracker(filepath.Join(t.TempDir(), "outage.flag"))
	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}
	if err := tr.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !tr.ShouldNotify() {
		t.Error("after Clear, ShouldNotify should be true again")
	}
}

func TestOutageTracker_ClearOnAbsentIsNoError(t *testing.T) {
	t.Parallel()
	tr := NewOutageTracker(filepath.Join(t.TempDir(), "outage.flag"))
	// Never marked; clearing should still succeed.
	if err := tr.Clear(); err != nil {
		t.Errorf("Clear on absent file errored: %v", err)
	}
}

func TestOutageTracker_CreatesParentDir(t *testing.T) {
	t.Parallel()
	nestedDir := filepath.Join(t.TempDir(), "deep", "subdir")
	tr := NewOutageTracker(filepath.Join(nestedDir, "outage.flag"))
	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}
	st, err := os.Stat(nestedDir)
	if err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
	if !st.IsDir() {
		t.Errorf("parent path is not a dir: %v", st)
	}
}

func TestOutageTracker_MarkIsIdempotent(t *testing.T) {
	t.Parallel()
	tr := NewOutageTracker(filepath.Join(t.TempDir(), "outage.flag"))
	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("first MarkNotified: %v", err)
	}
	if err := tr.MarkNotified(); err != nil {
		t.Errorf("second MarkNotified should also succeed, got %v", err)
	}
}

func TestOutageTracker_ShouldNotifyAgainAfterTTL(t *testing.T) {
	t.Parallel()
	flag := filepath.Join(t.TempDir(), "outage.flag")
	tr := NewOutageTrackerWithRenotify(flag, 1*time.Hour)

	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}
	// Immediately after marking, we are inside the TTL.
	if tr.ShouldNotify() {
		t.Fatal("ShouldNotify true immediately after MarkNotified — TTL ignored")
	}

	// Age the flag file past the TTL via os.Chtimes — the only way
	// to time-travel deterministically without a clock injection.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(flag, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if !tr.ShouldNotify() {
		t.Error("after TTL elapses, ShouldNotify should permit a re-notification")
	}
}

func TestOutageTracker_MarkNotifiedRefreshesMtime(t *testing.T) {
	t.Parallel()
	flag := filepath.Join(t.TempDir(), "outage.flag")
	tr := NewOutageTrackerWithRenotify(flag, 1*time.Hour)

	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("first MarkNotified: %v", err)
	}
	// Pretend the flag got stale.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(flag, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// Re-marking must restart the TTL clock — otherwise a long outage
	// would keep emitting back-to-back notifications because each
	// MarkNotified saw a flag still aged past TTL.
	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("second MarkNotified: %v", err)
	}
	info, err := os.Stat(flag)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Errorf("MarkNotified did not refresh mtime; flag is %v old", time.Since(info.ModTime()))
	}
	if tr.ShouldNotify() {
		t.Error("after refresh, ShouldNotify should be false again")
	}
}

func TestOutageTracker_TTLZeroDisablesRenotify(t *testing.T) {
	t.Parallel()
	flag := filepath.Join(t.TempDir(), "outage.flag")
	tr := NewOutageTrackerWithRenotify(flag, 0)

	if err := tr.MarkNotified(); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}
	// Even after deeply-aging the flag, ShouldNotify must stay false
	// — TTL=0 is the explicit "legacy one-shot" mode.
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(flag, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if tr.ShouldNotify() {
		t.Error("TTL=0 must disable re-notification regardless of age")
	}
}
