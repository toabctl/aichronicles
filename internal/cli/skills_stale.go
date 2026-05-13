package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultSkillStaleWindow matches what hermes-agent observed in
// practice for "skill goes wrong → cascade of errors": ~10 minutes
// is long enough for the agent to thrash through retries before
// giving up, short enough that an unrelated failure later in the
// session doesn't get attributed to the skill.
const defaultSkillStaleWindow = 10 * time.Minute

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Inspect captured skill activity (frequency, staleness, ...)",
	}
	cmd.AddCommand(newSkillsStaleCmd())
	cmd.AddCommand(newSkillsImpactCmd())
	cmd.AddCommand(newSkillsEvolveCmd())
	return cmd
}

func newSkillsImpactCmd() *cobra.Command {
	var (
		since    time.Duration
		window   time.Duration
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "impact",
		Short: "Per-skill success rate over recent loads (positive view of the staleness signal)",
		Long: "Walks every skill_load extraction in the window and reports,\n" +
			"per skill, how many loads were followed by a tool_failure in\n" +
			"the same session within --window vs how many were not. Where\n" +
			"`skills stale` surfaces only the trouble skills, `skills impact`\n" +
			"shows the FULL distribution — including the 100%-success ones\n" +
			"— so you can see which skills are actually pulling their weight\n" +
			"and which are pure noise. The same signal feeds the propose\n" +
			"prompt's installed-skills enrichment so the model gets to\n" +
			"reason about success rates when deciding whether a new skill\n" +
			"is warranted (or whether an existing skill should be revised\n" +
			"instead).\n\n" +
			"The signal is conservative: only Claude's PostToolUseFailure\n" +
			"hook fills tool_failure events, so a high success rate doesn't\n" +
			"mean the skill is perfect — just that this signal hasn't\n" +
			"fired. Output is sorted most-loaded first.\n\n" +
			"Talks to aichronicles-api over its UDS (override with\n" +
			"--socket or $AICHRONICLES_API_SOCKET).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			if since <= 0 {
				since = 30 * 24 * time.Hour
			}
			if window <= 0 {
				window = defaultSkillStaleWindow
			}
			return runSkillsImpact(cmd.Context(), c, runSkillsImpactOpts{
				Since:  since,
				Window: window,
				Format: format,
				Out:    cmd.OutOrStdout(),
			})
		},
	}
	addFlexDurationFlag(cmd, &since, "since", 30*24*time.Hour,
		"only consider skill loads within this window (e.g. 24h, 7d, 30d)")
	addFlexDurationFlag(cmd, &window, "window", defaultSkillStaleWindow,
		"how long after a skill load to look for a tool_failure (e.g. 5m, 10m, 30m)")
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
	return cmd
}

type runSkillsImpactOpts struct {
	Since  time.Duration
	Window time.Duration
	Format OutputFormat
	Out    io.Writer
}

func runSkillsImpact(ctx context.Context, c *apiclient.Client, opts runSkillsImpactOpts) error {
	sinceMs := time.Now().Add(-opts.Since).UnixMilli()
	resp, err := c.SkillImpact(ctx, wire.SkillImpactRequest{
		SinceMs:  sinceMs,
		WindowMs: opts.Window.Milliseconds(),
	})
	if err != nil {
		return fmt.Errorf("load skill impact: %w", err)
	}
	return renderSkillImpact(opts.Out, resp.Skills, opts.Since, opts.Window, opts.Format)
}

// renderSkillImpact writes the impact report to out. Sister of
// renderSkillStaleness; same column layout intent (skill name on
// the left, counts in the middle, percentage on the right) so a
// user looking at both side-by-side has consistent muscle memory.
func renderSkillImpact(out io.Writer, rows []wire.SkillImpact, since, window time.Duration, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"since":  since.String(),
			"window": window.String(),
			"skills": rows,
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "skill impact — last %s, failure window %s after each load\n\n",
		since, window)
	if len(rows) == 0 {
		fmt.Fprintf(&b, "  no skill_load extractions in window — backfill with `aichronicles backfill-extractions --only=skill_load`\n")
		_, err := io.WriteString(out, b.String())
		return err
	}
	fmt.Fprintf(&b, "%-32s %6s %6s %8s  %s\n",
		"skill", "loads", "failed", "success", "last loaded")
	fmt.Fprintln(&b, strings.Repeat("-", 80))
	now := time.Now()
	for _, s := range rows {
		name := s.Name
		if len(name) > 32 {
			name = name[:29] + "..."
		}
		pct := int(s.SuccessRate * 100)
		last := relativeTimeOrDash(s.LastLoadedMs, now)
		fmt.Fprintf(&b, "%-32s %6d %6d %7d%%  %s\n",
			name, s.TotalLoads, s.FailedLoads, pct, last)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// relativeTimeOrDash wraps timefmt.Relative with the table-cell
// empty-state token "-". Distinct from timefmt.RelativeOrDash only
// in suppressing future timestamps (treats clock-skew futures as
// missing rather than rendering "future?"), so the "stale" column
// stays scannable.
func relativeTimeOrDash(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	if time.UnixMilli(ms).After(now) {
		return "-"
	}
	return timefmt.Relative(ms, now)
}

func newSkillsStaleCmd() *cobra.Command {
	var (
		since    time.Duration
		window   time.Duration
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "stale",
		Short: "Surface skills whose loads correlate with subsequent tool_failures",
		Long: "Walks every skill_load extraction in the window and flags\n" +
			"skills where a load was followed by a tool_failure event in\n" +
			"the same session within --window. The signal is conservative:\n" +
			"only Claude's PostToolUseFailure hook fills tool_failure events,\n" +
			"so a low rate doesn't mean the skill is healthy — just that\n" +
			"this signal hasn't fired. A consistently high rate is a strong\n" +
			"hint that the skill's instructions are wrong / outdated and\n" +
			"deserve a `skill_manage edit` pass.\n\n" +
			"Output is sorted most-likely-broken first.\n\n" +
			"Talks to aichronicles-api over its UDS (override with\n" +
			"--socket or $AICHRONICLES_API_SOCKET).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			if since <= 0 {
				since = 14 * 24 * time.Hour
			}
			if window <= 0 {
				window = defaultSkillStaleWindow
			}
			return runSkillsStale(cmd.Context(), c, runSkillsStaleOpts{
				Since:  since,
				Window: window,
				Format: format,
				Out:    cmd.OutOrStdout(),
			})
		},
	}
	addFlexDurationFlag(cmd, &since, "since", 14*24*time.Hour,
		"only consider skill loads within this window (e.g. 24h, 7d, 30d)")
	addFlexDurationFlag(cmd, &window, "window", defaultSkillStaleWindow,
		"how long after a skill load to look for a tool_failure (e.g. 5m, 10m, 30m)")
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
	return cmd
}

type runSkillsStaleOpts struct {
	Since  time.Duration
	Window time.Duration
	Format OutputFormat
	Out    io.Writer
}

func runSkillsStale(ctx context.Context, c *apiclient.Client, opts runSkillsStaleOpts) error {
	sinceMs := time.Now().Add(-opts.Since).UnixMilli()
	resp, err := c.SkillStaleness(ctx, wire.SkillStalenessRequest{
		SinceMs:  sinceMs,
		WindowMs: opts.Window.Milliseconds(),
	})
	if err != nil {
		return fmt.Errorf("load skill staleness: %w", err)
	}
	return renderSkillStaleness(opts.Out, resp.Skills, opts.Since, opts.Window, opts.Format)
}

// renderSkillStaleness writes the staleness report to out in the
// requested format. Empty report → "no stale-correlated skills"
// rather than an empty table.
func renderSkillStaleness(out io.Writer, rows []wire.SkillStaleness, since, window time.Duration, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"since":  since.String(),
			"window": window.String(),
			"skills": rows,
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "skill staleness — last %s, failure window %s after each load\n\n",
		since, window)
	if len(rows) == 0 {
		fmt.Fprintf(&b, "  no stale-correlated skills in window\n")
		_, err := io.WriteString(out, b.String())
		return err
	}
	fmt.Fprintf(&b, "%-32s %6s %6s %5s  %s\n",
		"skill", "stale", "total", "rate", "example sessions (8-char prefix)")
	fmt.Fprintln(&b, strings.Repeat("-", 96))
	for _, s := range rows {
		shortIDs := make([]string, 0, len(s.Examples))
		for _, id := range s.Examples {
			shortIDs = append(shortIDs, preview.ShortID(id))
		}
		name := s.Name
		if len(name) > 32 {
			name = name[:29] + "..."
		}
		pct := int(s.Rate * 100)
		fmt.Fprintf(&b, "%-32s %6d %6d %4d%%  %s\n",
			name, s.StaleLoads, s.TotalLoads, pct, strings.Join(shortIDs, ", "))
	}
	_, err := io.WriteString(out, b.String())
	return err
}
