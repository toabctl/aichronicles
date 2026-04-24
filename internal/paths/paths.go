// Package paths resolves aichronicles' on-disk locations with the XDG
// Base Directory spec in mind. It's the one place clients and daemon
// agree on defaults, so moving a file only takes one edit.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Socket returns the Unix domain socket path used for the ingest API.
// Sockets are ephemeral IPC — per XDG they belong under XDG_RUNTIME_DIR
// (typically /run/user/<uid>, tmpfs, cleared on reboot). When that is
// unset we fall back to $TMPDIR/aichronicles-<uid>/sock so we never
// silently drop to a persistent directory where a stale socket could
// outlive a reboot.
func Socket() (string, error) {
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "aichronicles", "sock"), nil
	}
	uid := os.Getuid()
	if uid < 0 {
		return "", fmt.Errorf("no XDG_RUNTIME_DIR and non-posix uid %d", uid)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("aichronicles-%d", uid), "sock"), nil
}

// EventLog returns the JSONL append-only event log path. This is
// persistent state — XDG_STATE_HOME is the right bucket, with the
// conventional ~/.local/state fallback.
func EventLog() (string, error) {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "aichronicles", "events.jsonl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "aichronicles", "events.jsonl"), nil
}

// ConfigFile returns the TOML config path under XDG_CONFIG_HOME
// (fallback ~/.config). Loading is the caller's concern; this function
// only resolves where the file lives.
func ConfigFile() (string, error) {
	if c := os.Getenv("XDG_CONFIG_HOME"); c != "" {
		return filepath.Join(c, "aichronicles", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "aichronicles", "config.toml"), nil
}
