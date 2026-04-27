package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newTeardownCronCmd() *cobra.Command {
	var unitDir string
	var yes bool
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Remove aichronicles systemd --user cron timers",
		Long: "Inverse of `setup cron`. Disables + stops every aichronicles\n" +
			"cron timer, deletes the unit files from ~/.config/systemd/user/,\n" +
			"and reloads the user manager. Idempotent: running when nothing\n" +
			"is installed is a no-op.\n\n" +
			"Dry-run by default; pass --yes to actually remove.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := unitDir
			if dir == "" {
				var err error
				dir, err = defaultSystemdUserDir()
				if err != nil {
					return err
				}
			}
			report, err := RemoveCronUnits(dir, execSystemctl, !yes)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&unitDir, "unit-dir", "",
		"systemd user-unit directory (default: ~/.config/systemd/user)")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"confirm the removal (required to invoke systemctl and delete files)")
	return cmd
}

// RemoveCronUnits is the inverse of InstallCronUnits: disable +
// stop the timers, delete the unit files, reload. Mirrors the
// shape of RemoveSystemdUnits.
func RemoveCronUnits(unitDir string, runner SystemctlRunner, dryRun bool) (string, error) {
	present, err := cronUnitsPresent(unitDir)
	if err != nil {
		return "", err
	}

	if dryRun {
		paths := make([]string, 0, len(present))
		for _, name := range present {
			paths = append(paths, filepath.Join(unitDir, name))
		}
		return formatCronTeardownReport(unitDir, paths, true), nil
	}

	// Disable timers (the units that have an [Install] block;
	// services don't). disable --now stops + clears the symlinks.
	var timersToDisable []string
	for _, name := range present {
		for _, t := range cronTimerUnits {
			if name == t {
				timersToDisable = append(timersToDisable, name)
			}
		}
	}
	if len(timersToDisable) > 0 {
		args := append([]string{"disable", "--now"}, timersToDisable...)
		if err := runner(args...); err != nil {
			return "", err
		}
	}

	var removed []string
	for _, name := range cronUnitFilenames {
		path := filepath.Join(unitDir, name)
		err := os.Remove(path)
		switch {
		case err == nil:
			removed = append(removed, path)
		case os.IsNotExist(err):
			// already gone, fine
		default:
			return "", fmt.Errorf("remove %s: %w", path, err)
		}
	}

	if err := runner("daemon-reload"); err != nil {
		return "", err
	}

	return formatCronTeardownReport(unitDir, removed, false), nil
}

func cronUnitsPresent(unitDir string) ([]string, error) {
	var present []string
	for _, name := range cronUnitFilenames {
		path := filepath.Join(unitDir, name)
		_, err := os.Stat(path)
		switch {
		case err == nil:
			present = append(present, name)
		case os.IsNotExist(err):
			// skip
		default:
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
	}
	return present, nil
}

func formatCronTeardownReport(unitDir string, paths []string, dryRun bool) string {
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	if len(paths) == 0 {
		return fmt.Sprintf("no aichronicles cron units found in %s\n", unitDir)
	}
	out := fmt.Sprintf("%s %d cron unit file(s) in %s:\n", verb, len(paths), unitDir)
	for _, p := range paths {
		out += "  " + p + "\n"
	}
	if dryRun {
		out += "\n(dry-run; pass --yes to actually remove)\n"
	}
	return out
}
