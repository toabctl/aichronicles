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
	// Renders back in the unit the flag advertises, not in hours —
	// see TestFlexDuration_StringRendersDaysReadably for why.
	if got, want := wrap.String(), "14d"; got != want {
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

// TestFlexDuration_StringRendersDaysReadably pins the help-text
// contract: a flag that advertises "e.g. 30d, 180d" must not print
// its default in hours.
//
// time.Duration.String() alone printed "(default 4320h0m0s)" under an
// example list written in days, leaving the reader to divide by 24 to
// check the default even matched an example. The five-year prune
// default would have rendered "43800h0m0s".
func TestFlexDuration_StringRendersDaysReadably(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * 365 * 24 * time.Hour, "1825d"}, // the prune default
		{180 * 24 * time.Hour, "180d"},
		{7 * 24 * time.Hour, "7d"},
		{24 * time.Hour, "1d"},
		// Sub-day and non-day-multiples keep Go's rendering.
		{10 * time.Minute, "10m0s"},
		{90 * time.Minute, "1h30m0s"},
		{36 * time.Hour, "36h0m0s"},
		{0, "0s"},
	}
	for _, tc := range cases {
		d := flexDuration(tc.in)
		if got := d.String(); got != tc.want {
			t.Errorf("flexDuration(%v).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFlexDuration_StringSetRoundTrips is the property that makes the
// above safe: whatever String renders, Set must accept back.
func TestFlexDuration_StringSetRoundTrips(t *testing.T) {
	t.Parallel()
	for _, in := range []time.Duration{
		5 * 365 * 24 * time.Hour,
		180 * 24 * time.Hour,
		24 * time.Hour,
		10 * time.Minute,
		90 * time.Minute,
		36 * time.Hour,
	} {
		var out flexDuration
		src := flexDuration(in)
		rendered := src.String()
		if err := out.Set(rendered); err != nil {
			t.Errorf("Set(%q) from %v: %v", rendered, in, err)
			continue
		}
		if time.Duration(out) != in {
			t.Errorf("round-trip lost value: %v -> %q -> %v", in, rendered, time.Duration(out))
		}
	}
}

// TestPruneDefault_IsAFiveYearBackstop guards the retention window
// against quietly shrinking back.
//
// aichronicles-cron-prune.service runs `prune --yes` with no
// --older-than, so this constant IS the scheduled deletion window.
// At the previous six months a weekly timer would have been steadily
// destroying the corpus that reflect, propose and induction reason
// over — in a memory system the old rows are the product, and disk is
// not the scarce resource.
func TestPruneDefault_IsAFiveYearBackstop(t *testing.T) {
	t.Parallel()
	const fiveYears = 5 * 365 * 24 * time.Hour
	if defaultPruneAge != fiveYears {
		t.Errorf("defaultPruneAge = %v, want %v (five years)", defaultPruneAge, fiveYears)
	}
	if defaultPruneAge < 365*24*time.Hour {
		t.Error("a sub-year default would make the weekly prune timer destructive")
	}
}
