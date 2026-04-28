package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
)

// defaultUnresolvedSince matches LoadUnresolvedForCwd's default —
// kept on the CLI side too so the help text doesn't lie when the
// store helper's default is chosen.
const defaultUnresolvedSince = 30 * 24 * time.Hour

// defaultUnresolvedMaxSessions / MaxItems mirror the store helper.
const (
	defaultUnresolvedMaxSessions = 5
	defaultUnresolvedMaxItems    = 5
)

func newUnresolvedCmd() *cobra.Command {
	var (
		cwd         string
		since       time.Duration
		maxSessions int
		maxItems    int
		dbPath      string
		formatIn    string
	)
	cmd := &cobra.Command{
		Use:   "unresolved",
		Short: "Print unresolved items from prior sessions in this cwd",
		Long: "Reads each prior session's latest summary in --cwd (defaults\n" +
			"to $PWD), pulls $.unresolved from the body, and prints one\n" +
			"line per item with the source session id and topic. The\n" +
			"output is shaped to be drop-in usable as a Claude Code\n" +
			"SessionStart hook — pipe stdout into the agent's context so\n" +
			"the new session picks up where prior ones left off.\n\n" +
			"Example SessionStart hook script:\n\n" +
			"  #!/bin/sh\n" +
			"  aichronicles unresolved --cwd \"$PWD\"\n\n" +
			"Filters: --since (default 30d), --max-sessions (default 5),\n" +
			"--max-items (default 5 per session). The defaults bound the\n" +
			"hook output so the new session isn't drowned in stale TODOs.\n" +
			"Output is empty (0 exit) when no unresolved items match — a\n" +
			"hook can pipe straight in without a length check.\n\n" +
			"Use --format=json for the structured form when wiring this\n" +
			"into something other than a context-injection hook.",
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

			if cwd == "" {
				wd, werr := os.Getwd()
				if werr != nil {
					return fmt.Errorf("get cwd: %w", werr)
				}
				cwd = wd
			}
			cwd = filepath.Clean(cwd)

			sinceMs := time.Now().Add(-since).UnixMilli()
			items, err := store.LoadUnresolvedForCwd(cmd.Context(), s.DB(),
				cwd, sinceMs, maxSessions, maxItems)
			if err != nil {
				return fmt.Errorf("load unresolved: %w", err)
			}
			return renderUnresolved(cmd.OutOrStdout(), cwd, items, format)
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "cwd to look up (defaults to $PWD)")
	addFlexDurationFlag(cmd, &since, "since", defaultUnresolvedSince,
		"only consider sessions whose ended_at is within this window (e.g. 7d, 30d)")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", defaultUnresolvedMaxSessions,
		"cap on the number of prior sessions to draw from")
	cmd.Flags().IntVar(&maxItems, "max-items", defaultUnresolvedMaxItems,
		"cap on the number of unresolved items per session")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// renderUnresolved writes the items in either text or JSON form.
//
// Text form is shaped for SessionStart-hook consumption — a
// preamble line names the cwd and time horizon, then one line
// per item with a short id + relative time + the item itself.
// The preamble prefix ("aichronicles:") makes the source
// recognisable when it lands in the agent's context window.
//
// Empty items + text format = a single "(no unresolved …)" line.
// A hook caller piping into the agent gets a one-line "all clear"
// signal rather than a confusing empty injection.
func renderUnresolved(out io.Writer, cwd string, items []store.UnresolvedItem, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"cwd":   cwd,
			"items": items,
		})
	}
	if len(items) == 0 {
		_, err := fmt.Fprintf(out, "aichronicles: no unresolved items from prior sessions in %s\n", cwd)
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "aichronicles: %d unresolved item(s) from prior sessions in %s\n",
		len(items), cwd)
	now := time.Now()
	for _, it := range items {
		when := relativeTimeOrAbsent(it.EndedAtMs, now)
		// Topic gives 1-line context for the item (the agent picks
		// up where the topic ended), small enough to keep the
		// hook output tight.
		topic := it.Topic
		if topic == "" {
			topic = "(no summary topic)"
		}
		fmt.Fprintf(&b, "  • [%s, %s] %s — %s\n",
			it.SessionShort, when, topic, it.Item)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// relativeTimeOrAbsent renders epoch-millis as "Nh ago"/"Nd ago",
// or "still active" when ms == 0 (session hasn't ended yet — the
// summary lives ahead of the close).
func relativeTimeOrAbsent(ms int64, now time.Time) string {
	if ms <= 0 {
		return "still active"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 0:
		return "future?"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return time.UnixMilli(ms).UTC().Format("2006-01-02")
	}
}
