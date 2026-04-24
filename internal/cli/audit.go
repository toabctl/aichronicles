package cli

import (
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
)

// auditSnippetRunes caps the per-row snippet printed by `audit`. Long
// assistant messages with a single leaked token deep inside would
// otherwise dominate the output.
const auditSnippetRunes = 120

func newAuditCmd() *cobra.Command {
	var (
		limit  int
		since  time.Duration
		dbPath string
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Scan stored events for credential patterns (read-only)",
		Long: "Runs the current credential detectors against every stored\n" +
			"event and reports matches. Use it to find leaks that predate\n" +
			"the redactor, or to validate that a new detector catches what\n" +
			"you expect. This command never modifies the store — see\n" +
			"`aichronicles scrub` for that.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved := dbPath
			if resolved == "" {
				p, err := paths.StorePath()
				if err != nil {
					return err
				}
				resolved = p
			}
			s, err := store.Open(resolved)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			opts := AuditOptions{Limit: limit}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			_, err = RunAudit(s, redact.Default(), opts, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max events to scan, newest first (0 = scan all)")
	cmd.Flags().DurationVar(&since, "since", 0, "only scan events with ts_source newer than this duration (e.g. 168h)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// AuditOptions controls audit-time filters. Zero values mean
// "no filter" — the whole events table is scanned.
type AuditOptions struct {
	SinceMs int64
	Limit   int
}

// AuditReport is the running tally returned alongside per-row output.
type AuditReport struct {
	Scanned       int
	Flagged       int
	TotalFindings int
	PatternHits   map[string]int
}

// RunAudit scans events.content_text with scanner and prints one
// tab-separated row per event that contains any finding. A summary
// report is returned for callers that want aggregate numbers.
//
// Row format:
//
//	sess8  ts_utc  kind  patterns,csv  snippet
func RunAudit(s *store.Store, scanner redact.Scanner, opts AuditOptions, out io.Writer) (*AuditReport, error) {
	sqlText, args := buildAuditSQL(opts)
	rows, err := s.DB().Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	report := &AuditReport{PatternHits: map[string]int{}}
	for rows.Next() {
		var (
			sess    string
			tsMs    sql.NullInt64
			kind    string
			content sql.NullString
		)
		if err := rows.Scan(&sess, &tsMs, &kind, &content); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		report.Scanned++
		if !content.Valid || content.String == "" {
			continue
		}
		findings := scanner.Scan(content.String)
		if len(findings) == 0 {
			continue
		}
		report.Flagged++
		report.TotalFindings += len(findings)

		names := uniquePatterns(findings)
		for _, n := range names {
			report.PatternHits[n]++
		}

		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			firstN(sess, 8),
			formatTsNullable(tsMs),
			kind,
			strings.Join(names, ","),
			auditSnippet(content.String, findings[0]),
		)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return report, nil
}

// buildAuditSQL returns a query that yields (session_id, ts, kind,
// content_text) ordered by most-recent-first. content_text is allowed
// to be NULL — some kinds (tool_result, session_start) legitimately
// have no content. The caller filters those out.
func buildAuditSQL(opts AuditOptions) (string, []any) {
	var filter strings.Builder
	var args []any
	if opts.SinceMs > 0 {
		filter.WriteString(` AND ts_source_ms >= ?`)
		args = append(args, opts.SinceMs)
	}
	q := `SELECT session_id, ts_source_ms, kind, content_text
		FROM events
		WHERE content_text IS NOT NULL` + filter.String() + `
		ORDER BY ts_source_ms DESC`
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	return q, args
}

func uniquePatterns(findings []redact.Finding) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		if _, ok := seen[f.Pattern]; ok {
			continue
		}
		seen[f.Pattern] = struct{}{}
		out = append(out, f.Pattern)
	}
	return out
}

// auditSnippet renders a short context window around the first finding
// so the operator can see where in the event the match occurred.
// Keeps newlines out of the one-line output.
func auditSnippet(content string, f redact.Finding) string {
	// Centre the snippet on the find.
	start := f.Start
	end := f.End
	if start < 0 {
		start = 0
	}
	if end > len(content) {
		end = len(content)
	}
	// Widen to ±auditSnippetRunes/2 worth of runes around the match.
	// Byte-offset math from regex, then convert through runes for
	// truncation so we never split a multibyte sequence.
	prefix := content[:start]
	suffix := content[end:]
	pre := []rune(prefix)
	post := []rune(suffix)
	padding := auditSnippetRunes / 2
	if len(pre) > padding {
		pre = append([]rune{'…'}, pre[len(pre)-padding:]...)
	}
	if len(post) > padding {
		post = append(post[:padding], '…')
	}
	hit := []rune(content[start:end])
	combined := string(pre) + string(hit) + string(post)
	combined = strings.ReplaceAll(combined, "\n", " ")
	combined = strings.ReplaceAll(combined, "\r", " ")
	combined = strings.ReplaceAll(combined, "\t", " ")
	if r := []rune(combined); len(r) > auditSnippetRunes {
		combined = string(r[:auditSnippetRunes]) + "…"
	}
	// Belt-and-suspenders: never emit the raw secret substring
	// verbatim in audit output — replace it with the marker form so
	// copy-pasting audit output to a ticket doesn't re-leak.
	return strings.Replace(combined, string(hit), "<"+f.Pattern+">", 1)
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
