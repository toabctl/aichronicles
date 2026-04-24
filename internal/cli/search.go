package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// maxSnippetRunes caps how much of content_text we print per hit. FTS
// can return multi-KB assistant turns; the CLI is for spotting
// matches, not reading them.
const maxSnippetRunes = 140

func newSearchCmd() *cobra.Command {
	var (
		limit     int
		kind      string
		sessionID string
		since     time.Duration
		dbPath    string
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search over captured envelopes",
		Long: "Runs an FTS5 MATCH against events.content_text and prints the\n" +
			"top hits one per line. Query syntax is SQLite FTS5 (phrases in\n" +
			"quotes, AND/OR/NOT, prefix with *).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			opts := SearchOptions{
				Query:     args[0],
				Kind:      kind,
				SessionID: sessionID,
				Limit:     limit,
			}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			return RunSearch(s, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "max number of hits")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by event kind (user_prompt, tool_use, …)")
	cmd.Flags().StringVar(&sessionID, "session", "", "filter by session id")
	cmd.Flags().DurationVar(&since, "since", 0, "only events within this duration (e.g. 24h, 7d)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// SearchOptions are the flag values passed to RunSearch. Exported so
// tests and future MCP wiring can drive the same search path.
type SearchOptions struct {
	Query     string
	Kind      string
	SessionID string
	SinceMs   int64
	Limit     int
}

// RunSearch executes an FTS5 query against the store and writes one
// hit per line to out. Empty query is an error because FTS5 would
// either error itself or return the whole corpus.
func RunSearch(s *store.Store, opts SearchOptions, out io.Writer) error {
	if strings.TrimSpace(opts.Query) == "" {
		return errors.New("search query must not be empty")
	}

	sqlText, args := buildSearchSQL(opts)
	rows, err := s.DB().Query(sqlText, args...)
	if err != nil {
		return fmt.Errorf("search query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			sessionID  string
			kind       string
			cwd        *string
			tsSourceMs int64
			content    *string
		)
		if err := rows.Scan(&sessionID, &kind, &cwd, &tsSourceMs, &content); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		_, _ = fmt.Fprintln(out, formatHit(sessionID, kind, deref(cwd), tsSourceMs, deref(content)))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}
	return nil
}

// buildSearchSQL composes the SQL + args for a SearchOptions value.
// Factored out so tests can exercise composition without touching SQLite.
func buildSearchSQL(opts SearchOptions) (string, []any) {
	var b strings.Builder
	b.WriteString(`SELECT e.session_id, e.kind, e.cwd, e.ts_source_ms, e.content_text
		FROM events_fts f JOIN events e ON e.rowid = f.rowid
		WHERE events_fts MATCH ?`)
	args := []any{opts.Query}

	if opts.Kind != "" {
		b.WriteString(` AND e.kind = ?`)
		args = append(args, opts.Kind)
	}
	if opts.SessionID != "" {
		b.WriteString(` AND e.session_id = ?`)
		args = append(args, opts.SessionID)
	}
	if opts.SinceMs > 0 {
		b.WriteString(` AND e.ts_source_ms >= ?`)
		args = append(args, opts.SinceMs)
	}

	b.WriteString(` ORDER BY rank LIMIT ?`)
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit)

	return b.String(), args
}

// formatHit renders one row as a single grep-like line. Columns are
// tab-separated so downstream tools (awk, column -t) can re-align.
func formatHit(sessionID, kind, cwd string, tsSourceMs int64, content string) string {
	ts := time.UnixMilli(tsSourceMs).UTC().Format("2006-01-02T15:04:05Z")
	sess := sessionID
	if len(sess) > 8 {
		sess = sess[:8]
	}
	return fmt.Sprintf("%s\t%s\t%-17s\t%s\t%s", ts, sess, kind, cwd, truncateSnippet(content))
}

// truncateSnippet flattens newlines and caps rune length so hits fit
// on a terminal line. Runes (not bytes) so multibyte UTF-8 doesn't
// split mid-character.
func truncateSnippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	runes := []rune(s)
	if len(runes) <= maxSnippetRunes {
		return s
	}
	return string(runes[:maxSnippetRunes]) + "…"
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
