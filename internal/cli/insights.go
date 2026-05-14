package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultInsightsWindow matches the default the api uses (and what
// people expect from a "what did I do this month" digest). Override
// with --since for a tighter or wider net.
const defaultInsightsWindow = 30 * 24 * time.Hour

func newInsightsCmd() *cobra.Command {
	var (
		since    time.Duration
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Cross-session usage digest (sessions, top tools, top skills, activity-by-hour)",
		Long: "Reads sessions, events, and skill_load extractions in a window\n" +
			"and prints an aggregated report: counters (sessions, events,\n" +
			"tool calls), top tools, top skills, an activity-by-hour\n" +
			"histogram, and the highest-event-count sessions.\n\n" +
			"No LLM call — pure SQL aggregation, fast even on large stores.\n" +
			"For LLM-derived analysis, see `reflect` and `propose`.\n\n" +
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
			window := since
			if window <= 0 {
				window = defaultInsightsWindow
			}
			return runInsights(cmd.Context(), c, runInsightsOpts{
				Since:  window,
				Format: format,
				Out:    cmd.OutOrStdout(),
			})
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultInsightsWindow,
		"only consider sessions/events within this window (e.g. 24h, 7d, 30d)")
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// runInsightsOpts groups runInsights' arguments. Tests construct one
// directly without cobra.
type runInsightsOpts struct {
	Since  time.Duration
	Format OutputFormat
	Out    io.Writer
}

func runInsights(ctx context.Context, c *apiclient.Client, opts runInsightsOpts) error {
	sinceMs := time.Now().Add(-opts.Since).UnixMilli()
	report, err := c.Insights(ctx, apiclient.InsightsRequest{SinceMs: sinceMs})
	if err != nil {
		return fmt.Errorf("load insights: %w", err)
	}
	return renderInsights(opts.Out, &report, opts.Format)
}

// renderInsights writes the report to out in the requested format.
// JSON gets the raw struct; text gets a hand-tuned digest layout.
func renderInsights(out io.Writer, r *wire.Insights, format OutputFormat) error {
	if format == FormatJSON {
		return emitJSON(out, r)
	}
	return renderInsightsText(out, r)
}

// renderInsightsText produces a fixed-width terminal digest, with
// every section omitted when its underlying slice is empty (no
// "Top Skills: (none)" placeholder lines — keeps the report tight
// for users who don't load skills).
func renderInsightsText(out io.Writer, r *wire.Insights) error {
	var b strings.Builder

	since := time.UnixMilli(r.Window.SinceMs).UTC().Format("2006-01-02")
	until := time.UnixMilli(r.Window.UntilMs).UTC().Format("2006-01-02")
	fmt.Fprintf(&b, "aichronicles insights — %s to %s (%d days)\n\n", since, until, r.Window.Days)

	if r.Overview.Sessions == 0 {
		fmt.Fprintf(&b, "  no sessions in window\n")
		_, err := io.WriteString(out, b.String())
		return err
	}

	// Overview block
	fmt.Fprintf(&b, "Overview\n")
	fmt.Fprintf(&b, "  sessions:        %d\n", r.Overview.Sessions)
	fmt.Fprintf(&b, "  events:          %d\n", r.Overview.Events)
	fmt.Fprintf(&b, "  tool calls:      %d  (across %d distinct tools)\n", r.Overview.ToolUses, r.Overview.DistinctTools)
	fmt.Fprintf(&b, "  user prompts:    %d\n", r.Overview.UserPrompts)
	fmt.Fprintf(&b, "  distinct skills: %d\n\n", r.Overview.DistinctSkills)

	// Top tools
	if len(r.TopTools) > 0 {
		fmt.Fprintf(&b, "Top tools\n")
		nameW := maxToolNameWidth(r.TopTools, 28)
		total := r.Overview.ToolUses
		for _, t := range r.TopTools {
			pct := 0.0
			if total > 0 {
				pct = 100 * float64(t.Count) / float64(total)
			}
			fmt.Fprintf(&b, "  %-*s  %6d  %5.1f%%\n", nameW, t.ToolName, t.Count, pct)
		}
		b.WriteByte('\n')
	}

	// Top skills
	if len(r.TopSkills) > 0 {
		fmt.Fprintf(&b, "Top skills\n")
		nameW := maxSkillNameWidth(r.TopSkills, 28)
		for _, s := range r.TopSkills {
			last := time.UnixMilli(s.LastUsedMs).UTC().Format("2006-01-02")
			fmt.Fprintf(&b, "  %-*s  %5d  last: %s\n", nameW, s.Name, s.Count, last)
		}
		b.WriteByte('\n')
	}

	// Activity by hour — bar chart
	if hasAnyHourActivity(r.ActivityByHour) {
		fmt.Fprintf(&b, "Activity by hour (UTC)\n")
		writeHourHistogram(&b, r.ActivityByHour)
		b.WriteByte('\n')
	}

	// Top sessions — short id, event count, cwd, prompt preview
	if len(r.TopSessions) > 0 {
		fmt.Fprintf(&b, "Top sessions (by event count)\n")
		for _, ts := range r.TopSessions {
			cwd := "-"
			if ts.Cwd != nil && *ts.Cwd != "" {
				cwd = *ts.Cwd
			}
			prompt := strings.TrimSpace(ts.FirstPrompt)
			prompt = collapseWhitespace(prompt)
			// Rune-based truncation: byte-slicing at [:57] can cut
			// a multibyte UTF-8 codepoint mid-sequence and produce
			// invalid bytes in the list output. truncateRunes
			// handles the boundary and appends the ellipsis.
			prompt = truncateRunes(prompt, 60)
			started := "-"
			if ts.StartedAtMs != nil {
				started = time.UnixMilli(*ts.StartedAtMs).UTC().Format("2006-01-02")
			}
			fmt.Fprintf(&b, "  %s  %5d events  %s\n", preview.ShortID(ts.SessionID), ts.EventCount, started)
			fmt.Fprintf(&b, "    cwd: %s\n", cwd)
			if prompt != "" {
				fmt.Fprintf(&b, "    %q\n", prompt)
			}
		}
	}

	_, err := io.WriteString(out, b.String())
	return err
}

// maxToolNameWidth picks a column width for the tool-name column,
// clamped to cap so absurdly long names don't blow up the layout.
func maxToolNameWidth(tools []wire.ToolUsage, cap int) int {
	w := 0
	for _, t := range tools {
		if n := len(t.ToolName); n > w {
			w = n
		}
	}
	if w > cap {
		return cap
	}
	if w < 8 {
		return 8
	}
	return w
}

func maxSkillNameWidth(skills []wire.SkillUsage, cap int) int {
	w := 0
	for _, s := range skills {
		if n := len(s.Name); n > w {
			w = n
		}
	}
	if w > cap {
		return cap
	}
	if w < 8 {
		return 8
	}
	return w
}

func hasAnyHourActivity(buckets []wire.HourBucket) bool {
	for _, b := range buckets {
		if b.Count > 0 {
			return true
		}
	}
	return false
}

// writeHourHistogram renders the 24-bucket array as a vertical
// list with a unicode-block-element bar per hour. Bar width is
// scaled to the busiest hour so the chart self-fits at any
// activity level.
func writeHourHistogram(b *strings.Builder, buckets []wire.HourBucket) {
	maxCount := 0
	for _, hb := range buckets {
		if hb.Count > maxCount {
			maxCount = hb.Count
		}
	}
	const barMax = 30
	for _, hb := range buckets {
		bar := ""
		if maxCount > 0 {
			n := (hb.Count * barMax) / maxCount
			if hb.Count > 0 && n == 0 {
				n = 1 // never elide a non-zero bucket
			}
			bar = strings.Repeat("█", n)
		}
		fmt.Fprintf(b, "  %02d  %-*s  %d\n", hb.Hour, barMax, bar, hb.Count)
	}
}

// collapseWhitespace squashes runs of whitespace (newlines,
// multiple spaces) into single spaces so a multi-line first
// prompt fits on one line in the report.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}
