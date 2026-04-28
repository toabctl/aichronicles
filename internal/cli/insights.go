package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
)

// defaultInsightsWindow matches the default Hermes /insights uses
// (and what people expect from a "what did I do this month"
// digest). Override with --since for a tighter or wider net.
const defaultInsightsWindow = 30 * 24 * time.Hour

func newInsightsCmd() *cobra.Command {
	var (
		since    time.Duration
		dbPath   string
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
			"For LLM-derived analysis, see `reflect` and `propose`.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			window := since
			if window <= 0 {
				window = defaultInsightsWindow
			}
			sinceMs := time.Now().Add(-window).UnixMilli()
			report, err := store.LoadInsights(cmd.Context(), s.DB(), sinceMs, store.InsightsLimits{})
			if err != nil {
				return fmt.Errorf("load insights: %w", err)
			}
			return renderInsights(cmd.OutOrStdout(), report, format)
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultInsightsWindow,
		"only consider sessions/events within this window (e.g. 24h, 7d, 30d)")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// renderInsights writes the report to out in the requested format.
// JSON gets the raw struct; text gets a hand-tuned digest layout.
func renderInsights(out io.Writer, r *store.InsightsReport, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	return renderInsightsText(out, r)
}

// renderInsightsText produces a fixed-width terminal digest, with
// every section omitted when its underlying slice is empty (no
// "Top Skills: (none)" placeholder lines — keeps the report tight
// for users who don't load skills).
func renderInsightsText(out io.Writer, r *store.InsightsReport) error {
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
			if ts.Cwd.Valid && ts.Cwd.String != "" {
				cwd = ts.Cwd.String
			}
			prompt := strings.TrimSpace(ts.FirstPrompt)
			prompt = collapseWhitespace(prompt)
			if len(prompt) > 60 {
				prompt = prompt[:57] + "..."
			}
			started := "-"
			if ts.StartedAtMs.Valid {
				started = time.UnixMilli(ts.StartedAtMs.Int64).UTC().Format("2006-01-02")
			}
			fmt.Fprintf(&b, "  %s  %5d events  %s\n", shortID(ts.SessionID), ts.EventCount, started)
			fmt.Fprintf(&b, "    cwd: %s\n", cwd)
			if prompt != "" {
				fmt.Fprintf(&b, "    %q\n", prompt)
			}
		}
	}

	_, err := io.WriteString(out, b.String())
	return err
}

// shortID is the same 8-char prefix used elsewhere in the CLI.
// Local copy to avoid depending on web-package types from cli.
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// maxToolNameWidth picks a column width for the tool-name column,
// clamped to cap so absurdly long names don't blow up the layout.
func maxToolNameWidth(tools []store.ToolUsage, cap int) int {
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

func maxSkillNameWidth(skills []store.SkillUsage, cap int) int {
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

func hasAnyHourActivity(buckets []store.HourBucket) bool {
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
func writeHourHistogram(b *strings.Builder, buckets []store.HourBucket) {
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
// prompt fits on one line in the report. ASCII-only — no unicode
// whitespace beyond ' ' and '\t' / '\n' need handling for
// transcript content.
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

// Compile-time assert RunInsights signature for parity with the
// other Run* functions if a caller wants to invoke programmatically
// without going through cobra. Currently unused outside the cobra
// command, but kept for symmetry with RunReflect / RunPropose.
var _ = func(ctx context.Context, s *store.Store, w io.Writer, since time.Duration, format OutputFormat) error {
	if since <= 0 {
		since = defaultInsightsWindow
	}
	r, err := store.LoadInsights(ctx, s.DB(), time.Now().Add(-since).UnixMilli(), store.InsightsLimits{})
	if err != nil {
		return err
	}
	return renderInsights(w, r, format)
}
