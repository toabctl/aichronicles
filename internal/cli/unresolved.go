package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
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
		sockFlag    string
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
			"Talks to aichronicles-api over its UDS (override with\n" +
			"--socket or $AICHRONICLES_API_SOCKET).\n\n" +
			"Filters: --since (default 30d), --max-sessions (default 5),\n" +
			"--max-items (default 5 per session). Output is empty (0\n" +
			"exit) when no unresolved items match — a hook can pipe\n" +
			"straight in without a length check.\n\n" +
			"Use --format=json for the structured form when wiring this\n" +
			"into something other than a context-injection hook.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			if cwd == "" {
				wd, werr := os.Getwd()
				if werr != nil {
					return fmt.Errorf("get cwd: %w", werr)
				}
				cwd = wd
			}
			cwd = filepath.Clean(cwd)
			return runUnresolved(cmd.Context(), c, runUnresolvedOpts{
				Cwd:         cwd,
				Since:       since,
				MaxSessions: maxSessions,
				MaxItems:    maxItems,
				Format:      format,
				Out:         cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "cwd to look up (defaults to $PWD)")
	addFlexDurationFlag(cmd, &since, "since", defaultUnresolvedSince,
		"only consider sessions whose ended_at is within this window (e.g. 7d, 30d)")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", defaultUnresolvedMaxSessions,
		"cap on the number of prior sessions to draw from")
	cmd.Flags().IntVar(&maxItems, "max-items", defaultUnresolvedMaxItems,
		"cap on the number of unresolved items per session")
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// runUnresolvedOpts groups the parameters runUnresolved needs.
// Tests construct one directly without cobra.
type runUnresolvedOpts struct {
	Cwd         string
	Since       time.Duration
	MaxSessions int
	MaxItems    int
	Format      OutputFormat
	Out         io.Writer
}

// runUnresolved is the dependency-injected core of the cobra
// command. Takes a configured *apiclient.Client and writes the
// rendered output to opts.Out. Used by both the cobra path and
// the package tests.
func runUnresolved(ctx context.Context, c *apiclient.Client, opts runUnresolvedOpts) error {
	sinceMs := time.Now().Add(-opts.Since).UnixMilli()
	resp, err := c.Unresolved(ctx, apiclient.UnresolvedRequest{
		Cwd:                opts.Cwd,
		SinceMs:            sinceMs,
		MaxSessions:        opts.MaxSessions,
		MaxItemsPerSession: opts.MaxItems,
	})
	if err != nil {
		return fmt.Errorf("load unresolved: %w", err)
	}
	return renderUnresolved(opts.Out, opts.Cwd, resp.Items, opts.Format)
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
func renderUnresolved(out io.Writer, cwd string, items []wire.UnresolvedItem, format OutputFormat) error {
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

// relativeTimeOrAbsent wraps timefmt.Relative with the CLI-specific
// empty-state token "still active" (a session that hasn't ended yet
// has its summary written ahead of close; ms == 0 means "not yet
// closed", not "missing").
func relativeTimeOrAbsent(ms int64, now time.Time) string {
	if ms <= 0 {
		return "still active"
	}
	return timefmt.Relative(ms, now)
}
