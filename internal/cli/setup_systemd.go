package cli

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

//go:embed assets/aichronicles.socket
var systemdSocketUnit []byte

//go:embed assets/aichronicles.service
var systemdServiceUnit []byte

// unitFilenames lists the two systemd --user units aichronicles owns,
// in dependency order (socket before service so a partial install
// lands in a functional state).
var unitFilenames = []string{
	"aichronicles.socket",
	"aichronicles.service",
}

func newSetupSystemdCmd() *cobra.Command {
	var unitDir string
	cmd := &cobra.Command{
		Use:   "systemd",
		Short: "Install socket-activated systemd --user units",
		Long: "Writes aichronicles.socket and aichronicles.service into\n" +
			"~/.config/systemd/user/, reloads the user manager, and enables\n" +
			"the socket so aichroniclesd starts on demand when a hook\n" +
			"connects. The service unit expects `aichroniclesd` to be\n" +
			"discoverable on systemd's user manager PATH.\n\n" +
			"Requires `systemctl` on PATH. Idempotent.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := unitDir
			if dir == "" {
				var err error
				dir, err = defaultSystemdUserDir()
				if err != nil {
					return err
				}
			}
			report, err := InstallSystemdUnits(dir, execSystemctl)
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

// SystemctlRunner executes `systemctl --user <args...>`. The default
// implementation shells out; tests supply a stub.
type SystemctlRunner func(args ...string) error

// execSystemctl is the production runner: invokes the real systemctl
// binary with --user prepended.
func execSystemctl(args ...string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found on PATH: %w", err)
	}
	full := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", full...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// InstallSystemdUnits is the unit-testable core of `setup systemd`:
// write the embedded unit files into unitDir, then reload and enable
// via the supplied runner. Safe to run repeatedly — file writes are
// atomic and systemd's enable is idempotent. Returns a human-readable
// summary for the CLI to print.
func InstallSystemdUnits(unitDir string, runner SystemctlRunner) (string, error) {
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		return "", fmt.Errorf("ensure unit dir: %w", err)
	}

	written := make([]string, 0, len(unitFilenames))
	for _, name := range unitFilenames {
		path := filepath.Join(unitDir, name)
		changed, err := writeIfDifferent(path, unitContent(name))
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
	if err := runner("enable", "--now", "aichronicles.socket"); err != nil {
		return "", err
	}

	return formatSystemdSetupReport(unitDir, written), nil
}

// unitContent returns the embedded bytes for a known unit filename.
// Panics on unknown names because the caller iterates unitFilenames;
// a mismatch is a programmer error caught immediately in tests.
func unitContent(name string) []byte {
	switch name {
	case "aichronicles.socket":
		return systemdSocketUnit
	case "aichronicles.service":
		return systemdServiceUnit
	default:
		panic("unknown unit: " + name)
	}
}

// writeIfDifferent writes data to path iff the file doesn't already
// have identical contents. Returns true when the file was written.
// The atomic-rename pattern keeps observers from seeing half-written
// files.
func writeIfDifferent(path string, data []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && equalBytes(existing, data) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read existing: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := io.Copy(tmp, strings.NewReader(string(data))); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return false, fmt.Errorf("rename: %w", err)
	}
	return true, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// defaultSystemdUserDir resolves ~/.config/systemd/user (XDG-aware).
func defaultSystemdUserDir() (string, error) {
	if c := os.Getenv("XDG_CONFIG_HOME"); c != "" {
		return filepath.Join(c, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func formatSystemdSetupReport(unitDir string, written []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "unit dir: %s\n", unitDir)
	if len(written) == 0 {
		fmt.Fprint(&b, "no changes: units already up to date\n")
	} else {
		for _, p := range written {
			fmt.Fprintf(&b, "wrote %s\n", p)
		}
	}
	fmt.Fprint(&b, "reloaded systemd user manager\n")
	fmt.Fprint(&b, "enabled + started aichronicles.socket")
	return b.String()
}
