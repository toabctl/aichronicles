package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newTeardownSystemdCmd() *cobra.Command {
	var unitDir string
	var yes bool
	cmd := &cobra.Command{
		Use:   "systemd",
		Short: "Remove aichronicles systemd --user units",
		Long: "Disables + stops both unit pairs (aichronicles-api.socket /\n" +
			".service for the daemon; aichronicles-web.socket / .service\n" +
			"for the web UI), deletes the unit files from\n" +
			"~/.config/systemd/user/, and reloads the user manager.\n" +
			"Also removes the legacy aichronicles.{socket,service} units\n" +
			"installed by older versions before the api rearchitecture.\n" +
			"Idempotent: running when nothing is installed is a no-op.\n\n" +
			"Runs in dry-run mode by default: it reports what would be\n" +
			"disabled and deleted without invoking systemctl or removing\n" +
			"files. Pass --yes to actually remove.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := unitDir
			if dir == "" {
				var err error
				dir, err = defaultSystemdUserDir()
				if err != nil {
					return err
				}
			}
			report, err := RemoveSystemdUnits(dir, execSystemctl, !yes)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&unitDir, "unit-dir", "", "systemd user-unit directory (default: ~/.config/systemd/user)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the removal (required to invoke systemctl and delete files)")
	return cmd
}

// RemoveSystemdUnits is the unit-testable inverse of InstallSystemdUnits.
// It disables any units that exist, deletes them, and reloads the user
// manager. Running on a clean state runs daemon-reload only so systemd
// sees a consistent view regardless of what was or wasn't installed.
//
// When dryRun is true the function only inspects the filesystem and
// reports what it would do — runner is never called, files are never
// touched. The returned report uses "would disable/remove" phrasing.
func RemoveSystemdUnits(unitDir string, runner SystemctlRunner, dryRun bool) (string, error) {
	present, err := unitsPresent(unitDir)
	if err != nil {
		return "", err
	}

	if dryRun {
		wouldRemove := make([]string, 0, len(present))
		for _, name := range present {
			wouldRemove = append(wouldRemove, filepath.Join(unitDir, name))
		}
		return formatSystemdTeardownReport(unitDir, wouldRemove, true), nil
	}

	if len(present) > 0 {
		args := append([]string{"disable", "--now"}, present...)
		if err := runner(args...); err != nil {
			return "", err
		}
	}

	var removed []string
	// Remove both current and legacy units in one pass so an
	// upgrade from an older install (aichronicles.{socket,service})
	// to the new aichronicles-api.{socket,service} cleans up the
	// orphan units that point at the deleted aichroniclesd binary.
	for _, name := range allManagedUnitFilenames() {
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

	return formatSystemdTeardownReport(unitDir, removed, false), nil
}

// allManagedUnitFilenames returns every unit filename teardown
// is willing to remove: the current set plus the legacy set.
func allManagedUnitFilenames() []string {
	out := make([]string, 0, len(unitFilenames)+len(legacyUnitFilenames))
	out = append(out, unitFilenames...)
	out = append(out, legacyUnitFilenames...)
	return out
}

// unitsPresent returns the names (not paths) of our units that exist
// in unitDir. Used to avoid asking systemctl to disable a unit file
// that does not exist, which on older systemd versions errors.
// Includes legacy unit names so an upgrade-time teardown disables
// both the current and the soon-to-be-removed previous set.
func unitsPresent(unitDir string) ([]string, error) {
	var present []string
	for _, name := range allManagedUnitFilenames() {
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

func formatSystemdTeardownReport(unitDir string, removed []string, dryRun bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "unit dir: %s\n", unitDir)
	if len(removed) == 0 {
		fmt.Fprint(&b, "no changes: units not installed")
		if !dryRun {
			fmt.Fprint(&b, "\nreloaded systemd user manager")
		}
		return b.String()
	}
	if dryRun {
		fmt.Fprint(&b, "would disable + stop units\n")
		for _, p := range removed {
			fmt.Fprintf(&b, "would remove %s\n", p)
		}
		fmt.Fprint(&b, "(dry-run — pass --yes to apply)")
		return b.String()
	}
	fmt.Fprint(&b, "disabled + stopped units\n")
	for _, p := range removed {
		fmt.Fprintf(&b, "removed %s\n", p)
	}
	fmt.Fprint(&b, "reloaded systemd user manager")
	return b.String()
}
