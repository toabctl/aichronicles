package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// flexDurationPattern matches one Nd token (decimal allowed,
// e.g. "1.5d") so parseFlexDuration can rewrite each into its
// hour-equivalent before time.ParseDuration consumes the rest.
//
// We deliberately do NOT add weeks/months/years: those calendar
// units are ambiguous (which month? leap years?) and the CLI's
// hot path is "last N hours / last N days" anyway. If a user
// types `--since 2w` they should get a clear "unknown unit"
// error from time.ParseDuration rather than a silently-wrong
// fixed-week assumption.
//
// No \b anchor: a duration string only ever contains digits
// plus unit letters (h, m, s, plus ms / us / ns), and none of
// those include the letter d. An unanchored match is both safe
// and required to handle combined inputs like "1d12h" where
// \b between the d and the next digit fails (both are word
// chars).
var flexDurationPattern = regexp.MustCompile(`(\d+(?:\.\d+)?)d`)

// parseFlexDuration parses time.Duration with one extra suffix
// unit that Go's time.ParseDuration does NOT support but every
// CLI user expects:
//
//	Nd  → N*24h          (days)
//
// Everything else (h, m, s, ms, …) is delegated to
// time.ParseDuration unchanged. Combinations work as you'd
// expect: "1d12h" = 36h. Decimal scalars are accepted
// ("1.5d" = 36h) for symmetry with Go.
//
// Empty input is rejected with the same shape of error
// time.ParseDuration produces, so callers don't need a separate
// empty-check.
func parseFlexDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("parse duration %q: empty", s)
	}
	rewritten := flexDurationPattern.ReplaceAllStringFunc(s, func(token string) string {
		// token is e.g. "7d" — strip the trailing 'd' and convert.
		num := token[:len(token)-1]
		n, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return token
		}
		hours := n * 24
		// Format with limited precision so 1.5d becomes "36h"
		// (clean) rather than "36.000000h" (ugly but still valid).
		// strconv.FormatFloat with -1 precision picks the shortest
		// representation that round-trips.
		return strconv.FormatFloat(hours, 'f', -1, 64) + "h"
	})
	d, err := time.ParseDuration(rewritten)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}

// flexDuration is the pflag.Value adapter that lets cobra commands
// accept Nd / Nw on a --since (or any other) flag while the Go
// side reads a plain time.Duration. Use addFlexDurationFlag to
// register one with default sensible help text.
type flexDuration time.Duration

func (d *flexDuration) String() string { return time.Duration(*d).String() }

func (d *flexDuration) Set(s string) error {
	v, err := parseFlexDuration(s)
	if err != nil {
		return err
	}
	*d = flexDuration(v)
	return nil
}

// Type is the flag-type name pflag prints in --help. We claim
// "duration" so the help text reads identically to a normal
// DurationVar flag — the extra unit support is a superset of the
// stdlib, so users who never learn about Nd / Nw aren't confused
// by an unfamiliar flag-type name.
func (d *flexDuration) Type() string { return "duration" }

// addFlexDurationFlag registers a duration flag whose value backs a
// caller-owned time.Duration. Mirrors pflag's DurationVar shape so
// adoption at each call site is a one-line swap.
func addFlexDurationFlag(cmd *cobra.Command, dst *time.Duration, name string, def time.Duration, usage string) {
	*dst = def
	wrapper := (*flexDuration)(dst)
	cmd.Flags().Var(wrapper, name, usage)
}
