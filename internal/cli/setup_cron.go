package cli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Embedded unit files for the cron timers aichronicles ships. New
// timers slot in by adding the filename to cronUnitFilenames and a
// matching //go:embed line. The set is intentionally short — this
// is "ship a known schedule" not "expose a CRUD on systemd
// timers." Users wanting an arbitrary timer should write their own
// unit; this command exists for the canonical aichronicles
// schedules we recommend.
//
//go:embed assets/aichronicles-cron-weekly-digest.timer
var cronWeeklyDigestTimer []byte

//go:embed assets/aichronicles-cron-weekly-digest.service
var cronWeeklyDigestService []byte

// cronUnitFilenames is the canonical install set, in dependency
// order (timer can be enabled only after the service is on disk).
var cronUnitFilenames = []string{
	"aichronicles-cron-weekly-digest.service",
	"aichronicles-cron-weekly-digest.timer",
}

// cronTimerUnits is the subset of cronUnitFilenames that ARE
// timers — used by setup/teardown to know what to enable / disable.
// Computed once so the install step can iterate without string
// suffix tests.
var cronTimerUnits = []string{
	"aichronicles-cron-weekly-digest.timer",
}

func newSetupCronCmd() *cobra.Command {
	var unitDir string
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Install systemd --user timers for aichronicles' canonical scheduled tasks",
		Long: "Writes a fixed, opinionated set of systemd --user units into\n" +
			"~/.config/systemd/user/, reloads the user manager, and enables\n" +
			"each timer.\n\n" +
			"Today this is one timer: a weekly digest (`aichronicles digest\n" +
			"weekly`) that runs every Monday 09:00 UTC. Future canonical\n" +
			"schedules (e.g. nightly prune) slot in here without changing\n" +
			"the install command.\n\n" +
			"List installed timers with `systemctl --user list-timers`.\n" +
			"Trigger an immediate run with `systemctl --user start\n" +
			"<unit>.service`. Remove with `aichronicles teardown cron`.\n\n" +
			"Idempotent. Requires `systemctl` on PATH.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := unitDir
			if dir == "" {
				var err error
				dir, err = defaultSystemdUserDir()
				if err != nil {
					return err
				}
			}
			report, err := InstallCronUnits(dir, execSystemctl)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&unitDir, "unit-dir", "",
		"systemd user-unit directory (default: ~/.config/systemd/user)")
	return cmd
}

// cronUnitContent maps a known filename to its embedded bytes.
// Mirrors unitContent in setup_systemd.go; panics on an unknown
// name because the caller iterates cronUnitFilenames so any
// mismatch is a programmer error caught at first run.
func cronUnitContent(name string) []byte {
	switch name {
	case "aichronicles-cron-weekly-digest.timer":
		return cronWeeklyDigestTimer
	case "aichronicles-cron-weekly-digest.service":
		return cronWeeklyDigestService
	default:
		panic("unknown cron unit: " + name)
	}
}

// InstallCronUnits writes the embedded cron unit files into
// unitDir, reloads systemd, and enables each timer. Same shape
// as InstallSystemdUnits — the runner is a SystemctlRunner so
// tests can stub it without shelling out to a real systemctl.
//
// Idempotent: writes only when the on-disk content differs;
// re-enabling an already-enabled timer is a no-op for systemd.
func InstallCronUnits(unitDir string, runner SystemctlRunner) (string, error) {
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		return "", fmt.Errorf("ensure unit dir: %w", err)
	}

	written := make([]string, 0, len(cronUnitFilenames))
	for _, name := range cronUnitFilenames {
		path := filepath.Join(unitDir, name)
		changed, err := writeIfDifferent(path, cronUnitContent(name))
		if err != nil {
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if changed {
			written = append(written, path)
		}
	}

	if err := runner("daemon-reload"); err != nil {
		return "", err
	}
	for _, timer := range cronTimerUnits {
		if err := runner("enable", "--now", timer); err != nil {
			return "", err
		}
	}

	return formatCronInstallReport(unitDir, written), nil
}

// formatCronInstallReport renders the user-facing summary. List
// what was newly written + the conventional list/trigger commands
// so the user knows the operational handles after install.
func formatCronInstallReport(unitDir string, written []string) string {
	lead := fmt.Sprintf("installed cron units in %s\n", unitDir)
	if len(written) == 0 {
		lead += "  (no changes — units already up to date)\n"
	} else {
		for _, p := range written {
			lead += "  wrote " + p + "\n"
		}
	}
	lead += "\nUseful commands:\n"
	lead += "  systemctl --user list-timers          # see when each timer next fires\n"
	lead += "  systemctl --user start <unit>.service # trigger one immediate run\n"
	lead += "  journalctl --user -u <unit>.service   # see output of past runs\n"
	return lead
}
