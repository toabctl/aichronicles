package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeSystemctlRunner records every call and replays canned
// errors. Mirrors the pattern used by setup_systemd_test.go so
// tests don't need a real systemctl.
type fakeSystemctlRunner struct {
	calls [][]string
	errOn map[string]error
}

func (f *fakeSystemctlRunner) run(args ...string) error {
	f.calls = append(f.calls, args)
	if e, ok := f.errOn[strings.Join(args, " ")]; ok {
		return e
	}
	return nil
}

func TestInstallCronUnits_WritesEmbeddedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeSystemctlRunner{errOn: map[string]error{}}

	report, err := InstallCronUnits(dir, f.run)
	if err != nil {
		t.Fatalf("InstallCronUnits: %v", err)
	}
	for _, name := range cronUnitFilenames {
		path := filepath.Join(dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("missing unit file %s: %v", path, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("empty unit file %s", path)
		}
	}
	for _, want := range []string{
		"installed cron units",
		"systemctl --user list-timers",
		"systemctl --user start <unit>.service",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n%s", want, report)
		}
	}

	wantCalls := [][]string{
		{"daemon-reload"},
		{"enable", "--now", "aichronicles-cron-weekly-digest.timer"},
		{"enable", "--now", "aichronicles-cron-induction.timer"},
		{"enable", "--now", "aichronicles-cron-meta-analysis.timer"},
	}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Errorf("systemctl calls: got %v, want %v", f.calls, wantCalls)
	}
}

func TestInstallCronUnits_NoOpOnRerun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeSystemctlRunner{errOn: map[string]error{}}

	if _, err := InstallCronUnits(dir, f.run); err != nil {
		t.Fatalf("first install: %v", err)
	}
	preCalls := len(f.calls)

	report, err := InstallCronUnits(dir, f.run)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !strings.Contains(report, "no changes") {
		t.Errorf("rerun report missing 'no changes':\n%s", report)
	}
	// daemon-reload + enable still fire (idempotent on systemd's
	// side); only the file writes are skipped. So we expect calls
	// to grow by 1 + len(cronTimerUnits) — one daemon-reload plus
	// one enable per timer.
	want := 1 + len(cronTimerUnits)
	if got := len(f.calls) - preCalls; got != want {
		t.Errorf("rerun systemctl calls: got %d, want %d", got, want)
	}
}

func TestRemoveCronUnits_DryRunListsButDoesNotDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeSystemctlRunner{errOn: map[string]error{}}
	if _, err := InstallCronUnits(dir, f.run); err != nil {
		t.Fatalf("install: %v", err)
	}
	f.calls = nil

	report, err := RemoveCronUnits(dir, f.run, true)
	if err != nil {
		t.Fatalf("dry-run remove: %v", err)
	}
	if !strings.Contains(report, "would remove") {
		t.Errorf("dry-run report missing 'would remove':\n%s", report)
	}
	for _, name := range cronUnitFilenames {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("dry-run deleted %s: %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("dry-run made %d systemctl calls; expected 0", len(f.calls))
	}
}

func TestRemoveCronUnits_LiveDisablesAndDeletes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeSystemctlRunner{errOn: map[string]error{}}
	if _, err := InstallCronUnits(dir, f.run); err != nil {
		t.Fatalf("install: %v", err)
	}
	f.calls = nil

	report, err := RemoveCronUnits(dir, f.run, false)
	if err != nil {
		t.Fatalf("live remove: %v", err)
	}
	if !strings.Contains(report, "removed") {
		t.Errorf("report missing 'removed':\n%s", report)
	}
	for _, name := range cronUnitFilenames {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("file %s survived removal: %v", name, err)
		}
	}

	wantCalls := [][]string{
		{
			"disable", "--now",
			"aichronicles-cron-weekly-digest.timer",
			"aichronicles-cron-induction.timer",
			"aichronicles-cron-meta-analysis.timer",
		},
		{"daemon-reload"},
	}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Errorf("systemctl calls: got %v, want %v", f.calls, wantCalls)
	}
}

// TestRemoveCronUnits_NothingInstalledIsNoOp pins that running
// teardown without a prior install doesn't error and doesn't
// invoke disable on units that don't exist.
func TestRemoveCronUnits_NothingInstalledIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeSystemctlRunner{errOn: map[string]error{}}

	report, err := RemoveCronUnits(dir, f.run, false)
	if err != nil {
		t.Fatalf("RemoveCronUnits: %v", err)
	}
	if !strings.Contains(report, "no aichronicles cron units found") {
		t.Errorf("expected 'no units found' line:\n%s", report)
	}
	// Only a daemon-reload should fire (defensive). disable --now
	// must NOT be called when there's nothing to disable.
	for _, c := range f.calls {
		if len(c) >= 1 && c[0] == "disable" {
			t.Errorf("should not call disable when nothing is installed: %v", c)
		}
	}
}

// TestInstallCronUnits_PropagatesSystemctlError confirms a
// systemctl daemon-reload failure surfaces to the caller rather
// than being swallowed.
func TestInstallCronUnits_PropagatesSystemctlError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeSystemctlRunner{errOn: map[string]error{
		"daemon-reload": errors.New("systemctl boom"),
	}}
	if _, err := InstallCronUnits(dir, f.run); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected daemon-reload error, got %v", err)
	}
}

func TestCronUnitContent_PanicsOnUnknownName(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on unknown unit name")
		}
	}()
	cronUnitContent("not-a-real-unit.timer")
}

// TestWeeklyDigestTimer_IsAnchoredInUTC guards a silent, permanent
// data-correctness bug for users east of UTC+06:00.
//
// systemd.time(7): an OnCalendar= expression with no timezone is
// interpreted in LOCAL time. resolveWeekBounds anchors the digest
// window in UTC, so at a local offset beyond +06:00 the timer fires
// while it is still Sunday in UTC. mondayOf() then snaps to the
// previous Monday and the -7 lands two weeks back — the
// just-completed week is skipped, and because the shift recurs every
// Monday it is never picked up.
func TestWeeklyDigestTimer_IsAnchoredInUTC(t *testing.T) {
	t.Parallel()
	var found bool
	for _, line := range strings.Split(string(cronWeeklyDigestTimer), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "OnCalendar=") {
			continue
		}
		found = true
		if !strings.HasSuffix(line, " UTC") {
			t.Errorf("OnCalendar must end in \" UTC\"; %q is interpreted in local time "+
				"and skips a week for users east of UTC+06:00", line)
		}
	}
	if !found {
		t.Error("no OnCalendar= line in the weekly digest timer")
	}
}
