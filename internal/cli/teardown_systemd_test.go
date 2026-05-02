package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveSystemdUnits_NoUnitsInstalled_OnlyDaemonReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &recordingRunner{}

	report, err := RemoveSystemdUnits(dir, r.run, false)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(report, "not installed") {
		t.Errorf("report should note no-op: %q", report)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 systemctl call (daemon-reload), got %d: %v", len(r.calls), r.calls)
	}
	if got := strings.Join(r.calls[0], " "); got != "daemon-reload" {
		t.Errorf("got %q, want daemon-reload", got)
	}
}

func TestRemoveSystemdUnits_AfterInstallCleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := InstallSystemdUnits(dir, (&recordingRunner{}).run); err != nil {
		t.Fatalf("install: %v", err)
	}

	r := &recordingRunner{}
	report, err := RemoveSystemdUnits(dir, r.run, false)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}

	for _, name := range unitFilenames {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: expected absent, got err=%v", name, err)
		}
	}
	if !strings.Contains(report, "disabled + stopped") {
		t.Errorf("report should mention disable: %q", report)
	}

	// Two calls: disable+now+both units, then daemon-reload
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 systemctl calls, got %d: %v", len(r.calls), r.calls)
	}
	disable := strings.Join(r.calls[0], " ")
	if !strings.HasPrefix(disable, "disable --now ") {
		t.Errorf("first call should disable --now, got %q", disable)
	}
	for _, name := range unitFilenames {
		if !strings.Contains(disable, name) {
			t.Errorf("disable call missing %q: %q", name, disable)
		}
	}
}

func TestRemoveSystemdUnits_IsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := InstallSystemdUnits(dir, (&recordingRunner{}).run); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := RemoveSystemdUnits(dir, (&recordingRunner{}).run, false); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	report, err := RemoveSystemdUnits(dir, (&recordingRunner{}).run, false)
	if err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if !strings.Contains(report, "not installed") {
		t.Errorf("second remove should be no-op, got %q", report)
	}
}

func TestRemoveSystemdUnits_PartialStateCleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Only the socket unit exists; service unit is missing.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sockPath := filepath.Join(dir, "aichronicles-api.socket")
	if err := os.WriteFile(sockPath, systemdSocketUnit, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := &recordingRunner{}
	if _, err := RemoveSystemdUnits(dir, r.run, false); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket unit should be removed")
	}
	// disable call should reference only the socket
	disable := strings.Join(r.calls[0], " ")
	if !strings.Contains(disable, "aichronicles-api.socket") {
		t.Errorf("disable missing socket: %q", disable)
	}
	if strings.Contains(disable, "aichronicles-api.service") {
		t.Errorf("disable shouldn't reference absent service unit: %q", disable)
	}
}

func TestRemoveSystemdUnits_DryRun_DoesNotTouchFsOrRunSystemctl(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := InstallSystemdUnits(dir, (&recordingRunner{}).run); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Snapshot file names present before dry-run so we can verify
	// nothing was deleted.
	beforeEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	r := &recordingRunner{}
	report, err := RemoveSystemdUnits(dir, r.run, true)
	if err != nil {
		t.Fatalf("dry-run remove: %v", err)
	}
	if !strings.Contains(report, "would disable") {
		t.Errorf("dry-run report should say 'would disable', got %q", report)
	}
	if !strings.Contains(report, "pass --yes") {
		t.Errorf("dry-run report should hint at --yes, got %q", report)
	}
	if len(r.calls) != 0 {
		t.Errorf("dry-run invoked systemctl: %v", r.calls)
	}

	afterEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(beforeEntries) != len(afterEntries) {
		t.Fatalf("dry-run deleted files: before=%d after=%d", len(beforeEntries), len(afterEntries))
	}

	// A real remove after dry-run should still fully clean up.
	if _, err := RemoveSystemdUnits(dir, (&recordingRunner{}).run, false); err != nil {
		t.Fatalf("apply remove: %v", err)
	}
	for _, name := range unitFilenames {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s: expected absent after apply, got err=%v", name, err)
		}
	}
}

func TestRemoveSystemdUnits_DryRun_NothingInstalled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &recordingRunner{}
	report, err := RemoveSystemdUnits(dir, r.run, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(report, "not installed") {
		t.Errorf("expected no-op report, got %q", report)
	}
	if len(r.calls) != 0 {
		t.Errorf("dry-run no-op should not invoke systemctl: %v", r.calls)
	}
}

func TestRemoveSystemdUnits_RunnerErrorFromDisablePropagates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := InstallSystemdUnits(dir, (&recordingRunner{}).run); err != nil {
		t.Fatalf("install: %v", err)
	}
	r := &recordingRunner{err: errMockSystemctl}
	_, err := RemoveSystemdUnits(dir, r.run, false)
	if err == nil {
		t.Fatal("expected error when runner fails")
	}
}
