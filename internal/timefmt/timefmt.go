// Package timefmt centralises the relative- and absolute-time
// formatters that the CLI, web UI, and MCP tools all need.
//
// The same "Nm ago / Nh ago / Nd ago / YYYY-MM-DD" relative form
// was independently re-implemented in three places before this
// package existed; same for the absolute UTC form. Pinning the
// thresholds and output strings here keeps the surfaces visually
// consistent and makes a tweak (a different cutoff, a different
// date layout) a one-place edit.
//
// Empty-state handling (what to show when a timestamp is missing
// or zero) is intentionally NOT in this package: each caller has
// its own preferred token ("-", "active", "still active") and
// pre-checks before calling Relative / Absolute. RelativeOrDash
// and AbsoluteOrDash exist as the common shorthand.
package timefmt

import (
	"database/sql"
	"fmt"
	"time"
)

// Relative formats an epoch-millis timestamp as a short relative
// phrase ("just now", "5m ago", "3h ago", "2d ago"). Past 30 days
// it falls back to a UTC date so the output stays unambiguous —
// "2 months ago" loses too much resolution for a session list.
//
// Future times (clock skew, future-dated events) return "future?"
// rather than crashing or rendering nonsense. Caller is responsible
// for short-circuiting ms <= 0 with whatever empty-state token
// makes sense in context (see RelativeOrDash).
func Relative(ms int64, now time.Time) string {
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 0:
		return "future?"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return time.UnixMilli(ms).UTC().Format("2006-01-02")
	}
}

// RelativeOrDash is Relative with the common ms <= 0 → "-" fallback.
// Use when the empty-state token is just "-" (web list cells, CLI
// table columns).
func RelativeOrDash(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	return Relative(ms, now)
}

// Absolute renders epoch-millis as a human-readable UTC stamp:
// "2006-01-02 15:04 UTC". Used in the web detail page and CLI
// tables where the user is expected to read the timestamp directly.
func Absolute(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

// AbsoluteRFC3339 renders epoch-millis as the machine-readable
// RFC3339 form "2006-01-02T15:04:05Z". Used by MCP tool output
// where the consuming agent may want to re-parse the value.
func AbsoluteRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

// AbsoluteOrDash wraps Absolute for sql.NullInt64-typed timestamps
// (most callers reading sessions.started_at_ms / ended_at_ms),
// returning "-" when NULL or zero.
func AbsoluteOrDash(n sql.NullInt64) string {
	if !n.Valid || n.Int64 == 0 {
		return "-"
	}
	return Absolute(n.Int64)
}

// AbsoluteRFC3339OrDash is AbsoluteRFC3339 + the NullInt64 fallback.
func AbsoluteRFC3339OrDash(n sql.NullInt64) string {
	if !n.Valid {
		return "-"
	}
	return AbsoluteRFC3339(n.Int64)
}
