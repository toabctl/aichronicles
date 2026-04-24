package notify

import (
	"os"
	"path/filepath"
	"testing"
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
