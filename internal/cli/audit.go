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
	"github.com/toabctl/aichronicles/internal/wire"
)

func newAuditCmd() *cobra.Command {
	var (
		limit    int
		since    time.Duration
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Scan stored events for credential patterns (read-only)",
		Long: "Asks aichronicles-api to run the current credential detectors\n" +
			"against every stored event and prints one row per match. Use it\n" +
			"to find leaks that predate the redactor, or to validate that a\n" +
			"new detector catches what you expect. This command never\n" +
			"modifies the store — see `aichronicles scrub` for that.\n\n" +
			"The api runs the scanner server-side and returns the marker\n" +
			"form of every match — raw secret bytes never traverse the wire,\n" +
			"so audit output is safe to paste into a ticket.\n\n" +
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
			opts := AuditOptions{Limit: limit, Format: format}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			return runAudit(cmd.Context(), c, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max events to scan, newest first (0 = scan all)")
	addFlexDurationFlag(cmd, &since, "since", 0, "only scan events with ts_source newer than this duration (e.g. 24h, 7d)")
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// AuditOptions controls audit-time filters. Zero values mean
// "no filter" — the whole events table is scanned.
type AuditOptions struct {
	SinceMs int64
	Limit   int
	Format  OutputFormat // empty == FormatTable
}

// AuditFindingJSON is the JSON shape emitted by `audit --format=json`.
// Same field names as the api wire shape so jq pipelines built
// against /v1/audit work unchanged against the CLI output.
type AuditFindingJSON struct {
	SessionID  string   `json:"session_id"`
	TsSourceMs *int64   `json:"ts_source_ms"`
	Kind       string   `json:"kind"`
	Patterns   []string `json:"patterns"`
	Snippet    string   `json:"snippet"`
}

// AuditReportJSON is the top-level shape: a list of findings plus
// the aggregate counts. Lets jq pipelines either iterate
// `.findings[]` or `.scanned`/`.flagged` for the summary in one call.
type AuditReportJSON struct {
	Findings      []AuditFindingJSON `json:"findings"`
	Scanned       int                `json:"scanned"`
	Flagged       int                `json:"flagged"`
	TotalFindings int                `json:"total_findings"`
	PatternHits   map[string]int     `json:"pattern_hits"`
}

// runAudit calls /v1/audit and renders the response. Format=table
// emits a header + tab-aligned row per finding; format=json emits
// the AuditReportJSON envelope.
func runAudit(ctx context.Context, c *apiclient.Client, opts AuditOptions, out io.Writer) error {
	resp, err := c.Audit(ctx, wire.AuditRequest{SinceMs: opts.SinceMs, Limit: opts.Limit})
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}

	if opts.Format == FormatJSON {
		findings := make([]AuditFindingJSON, 0, len(resp.Findings))
		for _, f := range resp.Findings {
			findings = append(findings, AuditFindingJSON{
				SessionID:  f.SessionID,
				TsSourceMs: f.TsSourceMs,
				Kind:       f.Kind,
				Patterns:   f.Patterns,
				Snippet:    f.Snippet,
			})
		}
		return emitJSON(out, AuditReportJSON{
			Findings:      findings,
			Scanned:       resp.Scanned,
			Flagged:       resp.Flagged,
			TotalFindings: resp.TotalFindings,
			PatternHits:   resp.PatternHits,
		})
	}

	if resp.Flagged == 0 {
		_, err := fmt.Fprintln(out, "(no findings)")
		return err
	}

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SESSION\tWHEN\tKIND\tPATTERNS\tSNIPPET"); err != nil {
		return err
	}
	for _, f := range resp.Findings {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			firstN(f.SessionID, 8),
			formatTsPtr(f.TsSourceMs),
			f.Kind,
			strings.Join(f.Patterns, ","),
			f.Snippet,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = io.Copy(out, &buf)
	return err
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
