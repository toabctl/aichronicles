// Package notify sends freedesktop desktop notifications over DBus and
// tracks "daemon unreachable" outages so we alert once per outage
// rather than once per hook fire.
package notify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/godbus/dbus/v5"
)

// disableEnvVar lets tests (and anyone in a headless shell) force the
// noop notifier without touching config files.
const disableEnvVar = "AICHRONICLES_DISABLE_NOTIFY"

// Notifier sends a single freedesktop notification. Implementations
// must be safe to call from multiple goroutines.
type Notifier interface {
	Send(title, body string) error
}

// Noop returns a Notifier that silently drops every call. Useful for
// tests and for environments where notifications are undesirable.
func Noop() Notifier { return noopNotifier{} }

type noopNotifier struct{}

func (noopNotifier) Send(_, _ string) error { return nil }

// New returns a DBus-backed Notifier when enabled is true, DBus is
// available, and AICHRONICLES_DISABLE_NOTIFY is not set to "1". In
// every other case it returns a noop so callers never have to branch.
func New(enabled bool) Notifier {
	if !enabled || os.Getenv(disableEnvVar) == "1" {
		return noopNotifier{}
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		// Headless, no DBUS_SESSION_BUS_ADDRESS, systemd-less
		// container — nothing to send to, do not error.
		return noopNotifier{}
	}
	return &dbusNotifier{conn: conn}
}

type dbusNotifier struct {
	conn *dbus.Conn
}

const (
	fdoDest      = "org.freedesktop.Notifications"
	fdoPath      = "/org/freedesktop/Notifications"
	fdoInterface = "org.freedesktop.Notifications"
)

// Send posts a notification via org.freedesktop.Notifications.Notify.
// expire_timeout = -1 honors the user's desktop default.
func (n *dbusNotifier) Send(title, body string) error {
	obj := n.conn.Object(fdoDest, dbus.ObjectPath(fdoPath))
	call := obj.Call(
		fdoInterface+".Notify",
		0,                         // no call flags
		"aichronicles",            // app_name
		uint32(0),                 // replaces_id: 0 means new notification
		"",                        // app_icon (empty = none)
		title,                     // summary
		body,                      // body
		[]string{},                // actions (none)
		map[string]dbus.Variant{}, // hints (none)
		int32(-1),                 // expire_timeout: desktop default
	)
	if call.Err != nil {
		return fmt.Errorf("dbus Notify: %w", call.Err)
	}
	return nil
}

// OutageTracker remembers whether we've already notified about the
// current "daemon unreachable" outage via a marker file. Cleared on the
// first successful POST so a future outage gets its own notification.
//
// Races are fine: two concurrent hooks both seeing "absent" will both
// notify, which is one extra notification at worst, not a correctness
// bug. No cross-process locking attempted.
type OutageTracker struct {
	path string
}

// NewOutageTracker wraps a flag-file path. The caller picks the path;
// paths.OutageFlag() resolves the canonical location under
// XDG_RUNTIME_DIR.
func NewOutageTracker(path string) *OutageTracker {
	return &OutageTracker{path: path}
}

// ShouldNotify is true when no prior notification has been recorded
// for the current outage — i.e., the flag file does not exist.
func (t *OutageTracker) ShouldNotify() bool {
	_, err := os.Stat(t.path)
	return errors.Is(err, fs.ErrNotExist)
}

// MarkNotified creates the flag file so ShouldNotify returns false
// until Clear is called.
func (t *OutageTracker) MarkNotified() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return fmt.Errorf("ensure outage dir: %w", err)
	}
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create outage flag: %w", err)
	}
	return f.Close()
}

// Clear removes the flag file. Missing file is not an error — a clean
// state just means no outage was ever recorded.
func (t *OutageTracker) Clear() error {
	if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove outage flag: %w", err)
	}
	return nil
}
