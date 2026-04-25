package cli

import (
	"strings"
	"testing"
	"time"
)

func TestHumanRelative_BucketsByMagnitude(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		t    time.Time
		want string
	}{
		"30 seconds ago":  {t: now.Add(-30 * time.Second), want: "30s ago"},
		"5 minutes ago":   {t: now.Add(-5 * time.Minute), want: "5m ago"},
		"3 hours ago":     {t: now.Add(-3 * time.Hour), want: "3h ago"},
		"2 days ago":      {t: now.Add(-2 * 24 * time.Hour), want: "2d ago"},
		"45 days ago":     {t: now.Add(-45 * 24 * time.Hour), want: "1mo ago"},
		"in 5 minutes":    {t: now.Add(5 * time.Minute), want: "5m from now"},
		"sub-second is _": {t: now.Add(50 * time.Millisecond), want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := humanRelative(tc.t, now); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatTimeForUser_IncludesAbsoluteAndRelative(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.Local)
	tsMs := now.Add(-3 * time.Hour).UnixMilli()
	got := formatTimeForUser(tsMs, now)
	if !strings.Contains(got, "(3h ago)") {
		t.Errorf("expected '(3h ago)' in %q", got)
	}
	// Absolute portion is the layout we chose, no seconds.
	if !strings.Contains(got, "09:00") {
		t.Errorf("expected absolute time '09:00' in %q", got)
	}
}
