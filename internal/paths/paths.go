// Package paths resolves aichronicles' on-disk locations with the XDG
// Base Directory spec in mind. It's the one place clients and daemon
// agree on defaults, so moving a file only takes one edit.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// RuntimeDir returns the per-user ephemeral directory where sockets
// and outage flags live. XDG_RUNTIME_DIR when set (typically
// /run/user/<uid>, tmpfs, cleared on reboot), otherwise a UID-keyed
// fallback under $TMPDIR so we never silently drop ephemeral files
// into persistent state.
func RuntimeDir() (string, error) {
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "aichronicles"), nil
	}
	uid := os.Getuid()
	if uid < 0 {
		return "", fmt.Errorf("no XDG_RUNTIME_DIR and non-posix uid %d", uid)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("aichronicles-%d", uid)), nil
}

// Socket returns the Unix domain socket path for the legacy
// aichroniclesd ingest daemon. Kept for backward compatibility
// during the aichronicles-api transition; the new daemon uses
// APISocket().
func Socket() (string, error) {
	d, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "sock"), nil
}

// APISocket returns the Unix domain socket path for the unified
// aichronicles-api daemon (reads + writes + SSE + web HTML). Kept
// distinct from Socket() so the old and new daemons can coexist
// during the rollout window.
func APISocket() (string, error) {
	d, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "api.sock"), nil
}

// OutageFlag is the marker file the ingest CLI touches when it has
// already surfaced a "daemon unreachable" notification, so we only
// notify once per outage. Cleared on the next successful POST.
func OutageFlag() (string, error) {
	d, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "outage.flag"), nil
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

// PricesFile returns the TOML pricing-table path under
// XDG_CONFIG_HOME (fallback ~/.config). Used by `aichronicles usage`
// and the /usage web page to convert raw token counts into rough
// USD estimates. Same XDG layout as ConfigFile so a single config
// directory holds both files. Loading is the caller's concern.
func PricesFile() (string, error) {
	if c := os.Getenv("XDG_CONFIG_HOME"); c != "" {
		return filepath.Join(c, "aichronicles", "prices.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "aichronicles", "prices.toml"), nil
}

// EnvStore is the environment variable that overrides the SQLite
// store path when --db isn't given on the command line. Used by every
// subcommand and by [paths.ResolveStorePath].
const EnvStore = "AICHRONICLES_DB"

// EnvAPISocket is the environment variable that overrides the
// aichronicles-api daemon socket path. Distinct from EnvSocket
// (legacy aichroniclesd) so both can coexist during the
// transition.
const EnvAPISocket = "AICHRONICLES_API_SOCKET"

// EnvSocket is the environment variable that overrides the daemon
// Unix-socket path when --socket isn't given on the command line.
const EnvSocket = "AICHRONICLES_SOCKET"

// ResolveStorePath picks the SQLite store path with the conventional
// precedence: CLI flag > AICHRONICLES_DB env var > XDG default. Empty
// flag and empty env mean "use the XDG default."
func ResolveStorePath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(EnvStore); env != "" {
		return env, nil
	}
	return StorePath()
}

// ResolveSocketPath mirrors ResolveStorePath for the daemon UDS path.
// Precedence: CLI flag > AICHRONICLES_SOCKET env var > XDG default.
func ResolveSocketPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(EnvSocket); env != "" {
		return env, nil
	}
	return Socket()
}

// ResolveAPISocketPath mirrors ResolveSocketPath for the
// aichronicles-api daemon. Resolution order: --socket flag,
// $AICHRONICLES_API_SOCKET, then APISocket() default.
func ResolveAPISocketPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(EnvAPISocket); env != "" {
		return env, nil
	}
	return APISocket()
}

// StorePath returns the SQLite database path. Persistent state belongs
// under XDG_STATE_HOME (same bucket as events.jsonl), with the
// conventional ~/.local/state fallback.
func StorePath() (string, error) {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "aichronicles", "store.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "aichronicles", "store.db"), nil
}
