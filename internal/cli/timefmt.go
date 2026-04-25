package cli

import (
	"fmt"
	"time"
)

// humanTimeLayout is the canonical absolute-time format for table
// output. Local time, no seconds, no timezone offset — scannable for
// "what did I do this morning" use without dragging UTC offset math
// into the user's head. Tab-separated columns include both the
// absolute timestamp and a relative form (humanRelative) so users
// can pick whichever is faster to read.
const humanTimeLayout = "2006-01-02 15:04"

// formatTimeForUser renders ms as "<absolute>  <relative>" where
// relative is "5m ago", "3h ago", "2d ago", etc. now is the reference
// point — passed in (rather than calling time.Now() inline) so tests
// stay deterministic.
func formatTimeForUser(ms int64, now time.Time) string {
	t := time.UnixMilli(ms)
	abs := t.Local().Format(humanTimeLayout)
	rel := humanRelative(t, now)
	if rel == "" {
		return abs
	}
	return fmt.Sprintf("%s (%s)", abs, rel)
}

// humanRelative returns a short relative-time phrase like "5m ago",
// "in 3h", "2d ago". Granularity scales with magnitude — seconds for
// very recent times, days/months for older ones — so the phrase
// stays roughly the same width regardless of distance. Future times
// (clock skew, scheduled events) get an "in" prefix instead of "ago".
//
// Returns "" only when the absolute difference rounds below a second
// — at that point the user can read the absolute timestamp alone.
func humanRelative(t, now time.Time) string {
	d := now.Sub(t)
	suffix := "ago"
	if d < 0 {
		d = -d
		suffix = "from now"
	}

	switch {
	case d < time.Second:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds %s", int(d.Seconds()), suffix)
	case d < time.Hour:
		return fmt.Sprintf("%dm %s", int(d.Minutes()), suffix)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %s", int(d.Hours()), suffix)
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd %s", int(d.Hours()/24), suffix)
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo %s", int(d.Hours()/(24*30)), suffix)
	default:
		return fmt.Sprintf("%dy %s", int(d.Hours()/(24*365)), suffix)
	}
}
