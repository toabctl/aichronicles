package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/wire"
)

// maxPromptRunes caps the identifying-prompt snippet in `sessions`
// output so long opening prompts don't wreck the column layout.
const maxPromptRunes = 80

func newSessionsCmd() *cobra.Command {
	var (
		limit    int
		cwd      string
		since    time.Duration
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List sessions in the store, most-recently-ended first",
		Long: "One row per session, columns:\n\n" +
			"  SESSION  STARTED  ENDED  EVENTS  CWD  FIRST_PROMPT\n\n" +
			"On a TTY columns are aligned for reading; when piped or\n" +
			"redirected they emit as tab-separated values for awk/cut.\n" +
			"Pass --format=json for a structured payload suitable for jq.\n\n" +
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
			opts := SessionsOptions{
				Cwd:    cwd,
				Limit:  limit,
				Format: format,
			}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			return RunListSessions(cmd.Context(), c, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 30, "max sessions to return")
	cmd.Flags().StringVar(&cwd, "cwd", "", "filter by cwd (exact match)")
	addFlexDurationFlag(cmd, &since, "since", 0, "only sessions whose ended_at is within this duration (e.g. 24h, 7d)")
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// SessionsOptions is the filter set for listing sessions. Exported so
// tests can drive the same code path without going through cobra.
type SessionsOptions struct {
	Cwd     string
	SinceMs int64 // only sessions ended_at_ms >= this
	Limit   int
	Format  OutputFormat // empty == FormatTable
}

// SessionRowJSON is the JSON shape emitted by `sessions --format=json`.
// Field names are snake_case so they line up with the on-disk schema
// and with the rest of the JSON we emit (envelope, llm_outputs body).
// Nullable timestamps render as null rather than dash so jq pipelines
// can branch with `select(.ended_at_ms != null)`.
type SessionRowJSON struct {
	SessionID   string  `json:"session_id"`
	StartedAtMs *int64  `json:"started_at_ms"`
	EndedAtMs   *int64  `json:"ended_at_ms"`
	EventCount  int     `json:"event_count"`
	Cwd         *string `json:"cwd"`
	FirstPrompt *string `json:"first_prompt"`
}

// RunListSessions queries aichronicles-api and writes one row per
// session to out. Filters stack; ordering is always
// most-recently-ended first.
//
// Format=table (default) renders aligned columns + tab-separated
// underneath for awk/cut. Format=json emits a JSON array of
// SessionRowJSON values for jq pipelines. Empty result sets print a
// "(no sessions matched)" line in table mode and an empty array in
// JSON mode so consumers always see well-formed output.
func RunListSessions(ctx context.Context, c *apiclient.Client, opts SessionsOptions, out io.Writer) error {
	resp, err := c.Sessions(ctx, wire.SessionListRequest{
		SinceMs: opts.SinceMs,
		Cwd:     opts.Cwd,
		Limit:   opts.Limit,
	})
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if opts.Format == FormatJSON {
		payload := make([]SessionRowJSON, 0, len(resp.Sessions))
		for _, s := range resp.Sessions {
			payload = append(payload, SessionRowJSON{
				SessionID:   s.ID,
				StartedAtMs: s.StartedAtMs,
				EndedAtMs:   s.EndedAtMs,
				EventCount:  s.EventCount,
				Cwd:         s.Cwd,
				FirstPrompt: s.FirstPrompt,
			})
		}
		return emitJSON(out, payload)
	}

	if len(resp.Sessions) == 0 {
		_, err := fmt.Fprintln(out, "(no sessions matched)")
		return err
	}

	// Buffer through tabwriter so column widths line up across all
	// rows; otherwise streaming straight to out would lose alignment.
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SESSION\tSTARTED\tENDED\tEVENTS\tCWD\tFIRST_PROMPT"); err != nil {
		return err
	}
	for _, s := range resp.Sessions {
		_, _ = fmt.Fprintln(tw, formatSessionRow(s))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = io.Copy(out, &buf)
	return err
}

// formatSessionRow renders one row for CLI output. Tab-separated so
// downstream column -t / awk / cut behave. First prompt is truncated
// and newlines flattened; missing values render as "-".
func formatSessionRow(s wire.SessionDigest) string {
	sess := preview.ShortID(s.ID)
	return fmt.Sprintf(
		"%s\t%s\t%s\t%d\t%s\t%s",
		sess,
		formatTsPtr(s.StartedAtMs),
		formatTsPtr(s.EndedAtMs),
		s.EventCount,
		strPtrOrDash(s.Cwd),
		truncatePrompt(strPtrOrDash(s.FirstPrompt)),
	)
}

// formatTsPtr renders a *int64 epoch-millis as the canonical
// human-readable form, or "-" when nil.
func formatTsPtr(p *int64) string {
	if p == nil {
		return "-"
	}
	return formatTimeForUser(*p, time.Now())
}

// strPtrOrDash returns *p or "-" when p is nil or empty.
func strPtrOrDash(p *string) string {
	if p == nil || *p == "" {
		return "-"
	}
	return *p
}

// truncatePrompt collapses internal whitespace and caps the rune
// length so the FIRST_PROMPT column doesn't blow up the layout. The
// dash sentinel passes through unchanged.
func truncatePrompt(s string) string {
	if s == "-" {
		return s
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	runes := []rune(s)
	if len(runes) <= maxPromptRunes {
		return s
	}
	return string(runes[:maxPromptRunes]) + "…"
}
