package cli

import (
	"testing"
	"time"
)

func TestParseFlexDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want time.Duration
	}{
		// stdlib units pass through untouched
		{"168h", 168 * time.Hour},
		{"30m", 30 * time.Minute},
		{"15s", 15 * time.Second},
		{"1h30m", 90 * time.Minute},
		// the new Nd shorthand
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"14d", 14 * 24 * time.Hour},
		// fractional
		{"1.5d", 36 * time.Hour},
		// combined with stdlib units
		{"1d12h", 36 * time.Hour},
		{"3d6h", (3*24 + 6) * time.Hour},
		// negative
		{"-7d", -7 * 24 * time.Hour},
		// zero
		{"0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlexDuration(tc.in)
			if err != nil {
				t.Fatalf("parseFlexDuration(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseFlexDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseFlexDuration_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",      // empty
		"abc",   // not a duration
		"7days", // not a recognized unit
		"7 d",   // whitespace breaks the parser
		"d",     // no scalar
		"2w",    // weeks intentionally unsupported
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, err := parseFlexDuration(in); err == nil {
				t.Errorf("parseFlexDuration(%q) expected error, got nil", in)
			}
		})
	}
}

// TestFlexDurationFlag_RoundTrip confirms the pflag.Value adapter
// drives a real time.Duration through the cobra surface — what
// every callsite actually relies on.
func TestFlexDurationFlag_RoundTrip(t *testing.T) {
	t.Parallel()
	var since time.Duration
	wrap := (*flexDuration)(&since)
	if err := wrap.Set("14d"); err != nil {
		t.Fatalf("Set 14d: %v", err)
	}
	if since != 14*24*time.Hour {
		t.Errorf("after Set 14d: got %v, want %v", since, 14*24*time.Hour)
	}
	if got, want := wrap.String(), "336h0m0s"; got != want {
		t.Errorf("String: got %q, want %q", got, want)
	}
	// And weeks must not slip through — they're intentionally
	// unsupported because calendar-week semantics are ambiguous.
	if err := wrap.Set("2w"); err == nil {
		t.Error("expected Set 2w to error; weeks are intentionally unsupported")
	}
	if got, want := wrap.Type(), "duration"; got != want {
		t.Errorf("Type: got %q, want %q", got, want)
	}
}
