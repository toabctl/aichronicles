package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
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
		noDedup   bool
		formatIn  string
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search over captured envelopes",
		Long: "Runs an FTS5 MATCH against events.content_text and prints the\n" +
			"top hits one per line. Query syntax is SQLite FTS5 (phrases in\n" +
			"quotes, AND/OR/NOT, prefix with *). Pass --format=json for a\n" +
			"structured payload suitable for jq.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			// If the user passed a session-id prefix (e.g. the 8-char
			// preview `aichronicles sessions` prints), resolve it to
			// the full id here so the downstream filter is an exact
			// match.
			resolvedSessionID := sessionID
			if resolvedSessionID != "" {
				full, err := store.ResolveSessionIDPrefix(cmd.Context(), s.DB(), resolvedSessionID)
				if err != nil {
					return err
				}
				resolvedSessionID = full
			}

			opts := SearchOptions{
				Query:     args[0],
				Kind:      kind,
				SessionID: resolvedSessionID,
				Limit:     limit,
				NoDedup:   noDedup,
				Format:    format,
			}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			return RunSearch(s, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "max number of hits")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by event kind (user_prompt, tool_use, …)")
	cmd.Flags().StringVar(&sessionID, "session", "", "filter by session id or unique prefix")
	cmd.Flags().DurationVar(&since, "since", 0, "only events within this duration (e.g. 24h, 7d)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().BoolVar(&noDedup, "no-dedup", false, "show every row even when the same turn was captured from multiple sources (hook + import)")
	addFormatFlag(cmd, &formatIn)
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
	// NoDedup disables query-time deduplication. By default, when the
	// same logical turn is present from multiple sources (e.g. hook
	// events and transcript imports of the same session), search
	// collapses them to one row per (session_id, role, kind, content),
	// preferring transport=hook. Set to true to surface every row.
	NoDedup bool
	Format  OutputFormat // empty == FormatTable
}

// SearchHitJSON is the JSON shape emitted by `search --format=json`.
// Field names mirror the on-disk events schema. Snippet may be the
// full content_text or a truncation; the truncated flag tells the
// consumer which.
type SearchHitJSON struct {
	SessionID  string `json:"session_id"`
	Kind       string `json:"kind"`
	Cwd        string `json:"cwd,omitempty"`
	TsSourceMs int64  `json:"ts_source_ms"`
	Snippet    string `json:"snippet"`
	Truncated  bool   `json:"truncated"`
}

// RunSearch executes an FTS5 query against the store and writes hits
// to out. Empty query is an error because FTS5 would either error
// itself or return the whole corpus.
//
// Format=table renders aligned columns (header + tab-separated rows
// fed through tabwriter), with a "(no hits)" line on an empty result.
// Format=json emits a JSON array of SearchHitJSON values for jq.
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

	type rawHit struct {
		sessionID, kind string
		cwd, content    *string
		tsSourceMs      int64
	}
	var hits []rawHit
	for rows.Next() {
		var h rawHit
		if err := rows.Scan(&h.sessionID, &h.kind, &h.cwd, &h.tsSourceMs, &h.content); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	if opts.Format == FormatJSON {
		payload := make([]SearchHitJSON, 0, len(hits))
		for _, h := range hits {
			full := deref(h.content)
			snippet, truncated := snippetWithTruncation(full)
			payload = append(payload, SearchHitJSON{
				SessionID:  h.sessionID,
				Kind:       h.kind,
				Cwd:        deref(h.cwd),
				TsSourceMs: h.tsSourceMs,
				Snippet:    snippet,
				Truncated:  truncated,
			})
		}
		return emitJSON(out, payload)
	}

	if len(hits) == 0 {
		_, err := fmt.Fprintf(out, "(no hits for %q)\n", opts.Query)
		return err
	}

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "WHEN\tSESSION\tKIND\tCWD\tCONTENT"); err != nil {
		return err
	}
	for _, h := range hits {
		_, _ = fmt.Fprintln(tw, formatHit(h.sessionID, h.kind, deref(h.cwd), h.tsSourceMs, deref(h.content)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = io.Copy(out, &buf)
	return err
}

// snippetWithTruncation returns (snippet, truncated). Same rune cap
// and newline-flattening as truncateSnippet, but reports whether the
// cap actually fired so JSON consumers can distinguish "full content"
// from "first 140 runes." Keeps --format=table and --format=json
// telling the same story about how much of the content survived.
func snippetWithTruncation(s string) (string, bool) {
	flat := strings.ReplaceAll(s, "\n", " ")
	flat = strings.ReplaceAll(flat, "\r", " ")
	flat = strings.ReplaceAll(flat, "\t", " ")
	runes := []rune(flat)
	if len(runes) <= maxSnippetRunes {
		return flat, false
	}
	return string(runes[:maxSnippetRunes]), true
}

// buildSearchSQL composes the SQL + args for a SearchOptions value.
// Factored out so tests can exercise composition without touching SQLite.
//
// Default behavior wraps the base FTS result in a CTE + ROW_NUMBER
// window so logical turns captured through multiple sources (hook +
// transcript) collapse to one row per (session_id, role, kind,
// content_text). transport='hook' wins within each partition, then
// FTS rank breaks ties. ShowAll skips the wrapper and returns every
// row — useful for auditing what's actually in the store.
func buildSearchSQL(opts SearchOptions) (string, []any) {
	var filter strings.Builder
	args := []any{opts.Query}

	if opts.Kind != "" {
		filter.WriteString(` AND e.kind = ?`)
		args = append(args, opts.Kind)
	}
	if opts.SessionID != "" {
		filter.WriteString(` AND e.session_id = ?`)
		args = append(args, opts.SessionID)
	}
	if opts.SinceMs > 0 {
		filter.WriteString(` AND e.ts_source_ms >= ?`)
		args = append(args, opts.SinceMs)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	if opts.NoDedup {
		sql := `SELECT e.session_id, e.kind, e.cwd, e.ts_source_ms, e.content_text
			FROM events_fts f JOIN events e ON e.rowid = f.rowid
			WHERE events_fts MATCH ?` + filter.String() + `
			ORDER BY rank LIMIT ?`
		args = append(args, limit)
		return sql, args
	}

	// Deduped path. COALESCE on content_text so NULL partitions don't
	// collapse into a single group. Kind is included so tool_use and
	// assistant_message of a turn stay distinct even if they share
	// (session_id, role, content).
	sql := `WITH matched AS (
			SELECT e.rowid, e.session_id, e.role, e.kind, e.cwd,
				e.ts_source_ms, e.content_text, e.source_agent,
				(CASE
					WHEN json_extract(r.envelope_json, '$.transport') = 'hook'
					THEN 0 ELSE 1
				END) AS transport_rank,
				f.rank AS fts_rank
			FROM events_fts f
			JOIN events e         ON e.rowid = f.rowid
			JOIN raw_envelopes r  ON r.event_id = e.event_id
			WHERE events_fts MATCH ?` + filter.String() + `
		),
		ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY session_id, role, kind, COALESCE(content_text, rowid)
					ORDER BY transport_rank, fts_rank, rowid
				) AS rn
			FROM matched
		)
		SELECT session_id, kind, cwd, ts_source_ms, content_text
		FROM ranked
		WHERE rn = 1
		ORDER BY fts_rank
		LIMIT ?`
	args = append(args, limit)
	return sql, args
}

// formatHit renders one row as a tab-separated line. Column alignment
// is handled by the tabwriter in RunSearch; this function only
// prepares the cells.
func formatHit(sessionID, kind, cwd string, tsSourceMs int64, content string) string {
	ts := formatTimeForUser(tsSourceMs, time.Now())
	sess := sessionID
	if len(sess) > 8 {
		sess = sess[:8]
	}
	if cwd == "" {
		cwd = "-"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s", ts, sess, kind, cwd, truncateSnippet(content))
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
