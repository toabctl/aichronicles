// Package notify sends freedesktop desktop notifications over DBus and
// tracks "daemon unreachable" outages so we alert at most once per
// re-notify window — not once per hook fire (which would spam) and
// not exactly once forever (which let a 30-hour outage drop silently
// after the first dismissed notification).
package notify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

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

// DefaultOutageRenotify is how long we wait between desktop
// notifications for a single ongoing outage. Picked so a user who
// dismissed the first toast still hears about an outage that's been
// dropping events for an hour, without spamming a screen full of
// banners every 250ms hook fire.
const DefaultOutageRenotify = 1 * time.Hour

// OutageTracker remembers whether we've already notified about the
// current "daemon unreachable" outage via a marker file. Cleared on
// the first successful POST so a future outage gets its own
// notification. Re-notifies after RenotifyAfter elapses (using the
// flag file's mtime as the clock) so a long-running outage stays
// visible.
//
// Races are fine: two concurrent hooks both seeing "absent" or both
// seeing "expired" will both notify, which is one extra banner at
// worst, not a correctness bug. No cross-process locking attempted.
type OutageTracker struct {
	path          string
	renotifyAfter time.Duration
}

// NewOutageTracker wraps a flag-file path with the default re-notify
// window. paths.OutageFlag() resolves the canonical XDG_RUNTIME_DIR
// location.
func NewOutageTracker(path string) *OutageTracker {
	return NewOutageTrackerWithRenotify(path, DefaultOutageRenotify)
}

// NewOutageTrackerWithRenotify lets callers (mainly tests) override
// the re-notify TTL. Pass 0 to disable re-notification entirely —
// the legacy "one toast per outage" behaviour, retained behind an
// opt-in for symmetry but not recommended for interactive use.
func NewOutageTrackerWithRenotify(path string, renotifyAfter time.Duration) *OutageTracker {
	return &OutageTracker{path: path, renotifyAfter: renotifyAfter}
}

// ShouldNotify reports whether a fresh notification is in order.
// True when no prior notification has been recorded for the current
// outage (flag missing) OR when the prior notification is older
// than RenotifyAfter (so a long-running outage gets a periodic
// reminder rather than going silent forever).
func (t *OutageTracker) ShouldNotify() bool {
	info, err := os.Stat(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		// Permission or transient stat error — better to over-notify
		// than to silently swallow an outage. The caller's notifier
		// is itself best-effort, so an extra attempt here is cheap.
		return true
	}
	if t.renotifyAfter > 0 && time.Since(info.ModTime()) >= t.renotifyAfter {
		return true
	}
	return false
}

// MarkNotified creates or refreshes the flag file so ShouldNotify
// returns false until either Clear is called or RenotifyAfter
// elapses. O_TRUNC + a write are both required: opening with just
// O_CREATE leaves an existing file's mtime untouched, which would
// pin a long outage's clock at "first notified" forever.
func (t *OutageTracker) MarkNotified() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return fmt.Errorf("ensure outage dir: %w", err)
	}
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create outage flag: %w", err)
	}
	if _, err := f.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("write outage flag: %w", err)
	}
	return f.Close()
}

// Clear removes the flag file. Missing file is not an error — a
// clean state just means no outage was ever recorded.
func (t *OutageTracker) Clear() error {
	if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove outage flag: %w", err)
	}
	return nil
}
