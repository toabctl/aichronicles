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

//go:embed assets/aichronicles-api.socket
var systemdSocketUnit []byte

//go:embed assets/aichronicles-api.service
var systemdServiceUnit []byte

//go:embed assets/aichronicles-web.socket
var systemdWebSocketUnit []byte

//go:embed assets/aichronicles-web.service
var systemdWebServiceUnit []byte

// unitFilenames lists the systemd --user units aichronicles owns,
// in dependency order (socket before its service so a partial
// install lands in a functional state). Two pairs:
//
//   - aichronicles-api.socket / .service  — the unified daemon
//     (UDS, long-lived under default.target; serves reads + writes
//   - SSE + web HTML)
//   - aichronicles-web.socket / .service  — the web UI (TCP
//     loopback, socket-activated and idle-shutdown after 5 min of
//     no traffic)
//
// Both are enabled together by `aichronicles setup systemd` and
// removed together by `aichronicles teardown systemd`.
var unitFilenames = []string{
	"aichronicles-api.socket",
	"aichronicles-api.service",
	"aichronicles-web.socket",
	"aichronicles-web.service",
}

// activatedSockets lists every .socket unit setup enables. Kept
// separate from unitFilenames so the enable loop doesn't try to
// `enable` plain .service units (only sockets and timers want it).
var activatedSockets = []string{
	"aichronicles-api.socket",
	"aichronicles-web.socket",
}

// legacyUnitFilenames lists units this binary used to install
// before the aichronicles-api rearchitecture. Teardown removes
// these alongside the current set so an existing user install
// upgrades cleanly without leftover orphan units pointing at the
// deleted aichroniclesd binary.
var legacyUnitFilenames = []string{
	"aichronicles.socket",
	"aichronicles.service",
}

func newSetupSystemdCmd() *cobra.Command {
	var unitDir string
	cmd := &cobra.Command{
		Use:   "systemd",
		Short: "Install socket-activated systemd --user units",
		Long: "Writes the api and web-UI unit pairs into\n" +
			"~/.config/systemd/user/, reloads the user manager, and enables\n" +
			"both sockets so the matching service starts on demand when\n" +
			"someone connects:\n\n" +
			"  - aichronicles-api.socket    UDS for the unified daemon\n" +
			"  - aichronicles-api.service   the long-lived read+write+SSE+web daemon\n" +
			"  - aichronicles-web.socket    TCP 127.0.0.1:7878 for the web UI\n" +
			"  - aichronicles-web.service   web UI; idle-shutdown after 5m\n\n" +
			"The service units expect `aichronicles` and `aichronicles-api`\n" +
			"to be discoverable on systemd's user manager PATH (~/.local/bin\n" +
			"by default, via `make install`).\n\n" +
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
	// Enable both sockets in one call so systemd's reload sees a
	// consistent target state. enable --now is idempotent; running
	// again is a cheap no-op when the units are already enabled.
	enableArgs := append([]string{"enable", "--now"}, activatedSockets...)
	if err := runner(enableArgs...); err != nil {
		return "", err
	}

	return formatSystemdSetupReport(unitDir, written), nil
}

// unitContent returns the embedded bytes for a known unit filename.
// Panics on unknown names because the caller iterates unitFilenames;
// a mismatch is a programmer error caught immediately in tests.
func unitContent(name string) []byte {
	switch name {
	case "aichronicles-api.socket":
		return systemdSocketUnit
	case "aichronicles-api.service":
		return systemdServiceUnit
	case "aichronicles-web.socket":
		return systemdWebSocketUnit
	case "aichronicles-web.service":
		return systemdWebServiceUnit
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
	fmt.Fprintf(&b, "enabled + started %s", strings.Join(activatedSockets, ", "))
	return b.String()
}
