package timefmt

import (
	"database/sql"
	"testing"
	"time"
)

func TestRelative(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ms   int64
		want string
	}{
		{"future", now.Add(1 * time.Hour).UnixMilli(), "future?"},
		{"just now", now.Add(-30 * time.Second).UnixMilli(), "just now"},
		{"5m", now.Add(-5 * time.Minute).UnixMilli(), "5m ago"},
		{"3h", now.Add(-3 * time.Hour).UnixMilli(), "3h ago"},
		{"2d", now.Add(-2 * 24 * time.Hour).UnixMilli(), "2d ago"},
		{"40d falls back to date", now.Add(-40 * 24 * time.Hour).UnixMilli(), "2026-03-19"},
	}
	for _, c := range cases {
		if got := Relative(c.ms, now); got != c.want {
			t.Errorf("%s: Relative = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRelativeOrDash(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	if got := RelativeOrDash(0, now); got != "-" {
		t.Errorf("zero ms: got %q, want -", got)
	}
	if got := RelativeOrDash(-1, now); got != "-" {
		t.Errorf("negative ms: got %q, want -", got)
	}
	want := Relative(now.Add(-time.Hour).UnixMilli(), now)
	if got := RelativeOrDash(now.Add(-time.Hour).UnixMilli(), now); got != want {
		t.Errorf("non-zero: got %q, want %q", got, want)
	}
}

func TestAbsolute(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 4, 28, 13, 45, 7, 0, time.UTC).UnixMilli()
	if got := Absolute(ts); got != "2026-04-28 13:45 UTC" {
		t.Errorf("Absolute: got %q, want 2026-04-28 13:45 UTC", got)
	}
	if got := AbsoluteRFC3339(ts); got != "2026-04-28T13:45:07Z" {
		t.Errorf("AbsoluteRFC3339: got %q", got)
	}
}

func TestAbsoluteOrDash(t *testing.T) {
	t.Parallel()
	if got := AbsoluteOrDash(sql.NullInt64{Valid: false}); got != "-" {
		t.Errorf("invalid: got %q, want -", got)
	}
	if got := AbsoluteOrDash(sql.NullInt64{Valid: true, Int64: 0}); got != "-" {
		t.Errorf("zero: got %q, want -", got)
	}
	ts := time.Date(2026, 4, 28, 13, 45, 0, 0, time.UTC).UnixMilli()
	if got := AbsoluteOrDash(sql.NullInt64{Valid: true, Int64: ts}); got != "2026-04-28 13:45 UTC" {
		t.Errorf("valid: got %q", got)
	}
}
