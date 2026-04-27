package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
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
	return cmd
}

func newSkillsStaleCmd() *cobra.Command {
	var (
		since    time.Duration
		window   time.Duration
		dbPath   string
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
			"Output is sorted most-likely-broken first.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			resolved, err := paths.ResolveStorePath(dbPath)
			if err != nil {
				return err
			}
			s, err := store.Open(resolved)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			if since <= 0 {
				since = 14 * 24 * time.Hour
			}
			if window <= 0 {
				window = defaultSkillStaleWindow
			}
			sinceMs := time.Now().Add(-since).UnixMilli()
			report, err := store.LoadSkillStaleness(cmd.Context(), s.DB(),
				sinceMs, window.Milliseconds(),
				store.SkillStalenessLimits{})
			if err != nil {
				return fmt.Errorf("load skill staleness: %w", err)
			}
			return renderSkillStaleness(cmd.OutOrStdout(), report, since, window, format)
		},
	}
	addFlexDurationFlag(cmd, &since, "since", 14*24*time.Hour,
		"only consider skill loads within this window (e.g. 24h, 7d, 30d)")
	addFlexDurationFlag(cmd, &window, "window", defaultSkillStaleWindow,
		"how long after a skill load to look for a tool_failure (e.g. 5m, 10m, 30m)")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// renderSkillStaleness writes the staleness report to out in the
// requested format. Empty report → "no stale-correlated skills"
// rather than an empty table.
func renderSkillStaleness(out io.Writer, rows []store.SkillStaleness, since, window time.Duration, format OutputFormat) error {
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
			if len(id) > 8 {
				shortIDs = append(shortIDs, id[:8])
			} else {
				shortIDs = append(shortIDs, id)
			}
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
