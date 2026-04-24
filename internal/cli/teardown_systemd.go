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
	cmd := &cobra.Command{
		Use:   "systemd",
		Short: "Remove aichronicles systemd --user units",
		Long: "Disables + stops aichronicles.socket and aichronicles.service,\n" +
			"deletes the unit files from ~/.config/systemd/user/, and reloads\n" +
			"the user manager. Idempotent: running when nothing is installed\n" +
			"is a no-op.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := unitDir
			if dir == "" {
				var err error
				dir, err = defaultSystemdUserDir()
				if err != nil {
					return err
				}
			}
			report, err := RemoveSystemdUnits(dir, execSystemctl)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&unitDir, "unit-dir", "", "systemd user-unit directory (default: ~/.config/systemd/user)")
	return cmd
}

// RemoveSystemdUnits is the unit-testable inverse of InstallSystemdUnits.
// It disables any units that exist, deletes them, and reloads the user
// manager. Running on a clean state runs daemon-reload only so systemd
// sees a consistent view regardless of what was or wasn't installed.
func RemoveSystemdUnits(unitDir string, runner SystemctlRunner) (string, error) {
	present, err := unitsPresent(unitDir)
	if err != nil {
		return "", err
	}

	if len(present) > 0 {
		args := append([]string{"disable", "--now"}, present...)
		if err := runner(args...); err != nil {
			return "", err
		}
	}

	var removed []string
	for _, name := range unitFilenames {
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

	return formatSystemdTeardownReport(unitDir, removed), nil
}

// unitsPresent returns the names (not paths) of our units that exist
// in unitDir. Used to avoid asking systemctl to disable a unit file
// that does not exist, which on older systemd versions errors.
func unitsPresent(unitDir string) ([]string, error) {
	var present []string
	for _, name := range unitFilenames {
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

func formatSystemdTeardownReport(unitDir string, removed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "unit dir: %s\n", unitDir)
	if len(removed) == 0 {
		fmt.Fprint(&b, "no changes: units not installed\n")
		fmt.Fprint(&b, "reloaded systemd user manager")
		return b.String()
	}
	fmt.Fprint(&b, "disabled + stopped units\n")
	for _, p := range removed {
		fmt.Fprintf(&b, "removed %s\n", p)
	}
	fmt.Fprint(&b, "reloaded systemd user manager")
	return b.String()
}
