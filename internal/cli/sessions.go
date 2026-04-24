package cli

import (
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// maxPromptRunes caps the identifying-prompt snippet in `sessions`
// output so long opening prompts don't wreck the column layout.
const maxPromptRunes = 80

func newSessionsCmd() *cobra.Command {
	var (
		limit  int
		cwd    string
		agent  string
		since  time.Duration
		dbPath string
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List sessions in the store, most-recently-ended first",
		Long: "One tab-separated line per session:\n\n" +
			"  sess8  started_at  ended_at  event_count  cwd  first_prompt_snippet\n\n" +
			"Filters stack. Output is grep-friendly; pipe through column -t\n" +
			"for aligned columns.",
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

			opts := SessionsOptions{
				Cwd:   cwd,
				Agent: agent,
				Limit: limit,
			}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			return RunListSessions(s, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 30, "max sessions to return")
	cmd.Flags().StringVar(&cwd, "cwd", "", "filter by cwd (exact match)")
	cmd.Flags().StringVar(&agent, "agent", "", "filter by source_agent (e.g. claude-code)")
	cmd.Flags().DurationVar(&since, "since", 0, "only sessions ended within this duration (e.g. 24h, 7d)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// SessionsOptions is the filter set for listing sessions. Exported so
// tests and MCP wiring can drive the same code path.
type SessionsOptions struct {
	Cwd     string
	Agent   string
	SinceMs int64 // only sessions ended_at_ms >= this
	Limit   int
}

// RunListSessions queries the store and writes one row per session
// to out. Filters stack; ordering is always most-recently-ended first.
func RunListSessions(s *store.Store, opts SessionsOptions, out io.Writer) error {
	sqlText, args := buildSessionsSQL(opts)
	rows, err := s.DB().Query(sqlText, args...)
	if err != nil {
		return fmt.Errorf("sessions query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id          string
			startedMs   sql.NullInt64
			endedMs     sql.NullInt64
			eventCount  int
			cwd         sql.NullString
			firstPrompt sql.NullString
		)
		if err := rows.Scan(&id, &startedMs, &endedMs, &eventCount, &cwd, &firstPrompt); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		_, _ = fmt.Fprintln(out, formatSessionRow(id, startedMs, endedMs, eventCount, cwd, firstPrompt))
	}
	return rows.Err()
}

// buildSessionsSQL composes the list query. Factored out for unit
// tests that check filter composition without hitting SQLite.
func buildSessionsSQL(opts SessionsOptions) (string, []any) {
	var filter strings.Builder
	var args []any

	if opts.Cwd != "" {
		filter.WriteString(` AND s.cwd = ?`)
		args = append(args, opts.Cwd)
	}
	if opts.Agent != "" {
		filter.WriteString(` AND s.source_agent = ?`)
		args = append(args, opts.Agent)
	}
	if opts.SinceMs > 0 {
		filter.WriteString(` AND s.ended_at_ms >= ?`)
		args = append(args, opts.SinceMs)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	}
	args = append(args, limit)

	// The inner SELECT identifies each session by its first user_prompt
	// so the user can recognize what the session was about. Subquery
	// runs per row; at thousand-session scale it's still sub-ms.
	return `SELECT s.id, s.started_at_ms, s.ended_at_ms, s.event_count, s.cwd,
			(SELECT content_text FROM events
				WHERE session_id = s.id AND kind = 'user_prompt'
				ORDER BY ts_source_ms ASC LIMIT 1) AS first_prompt
		FROM sessions s
		WHERE 1=1` + filter.String() + `
		ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
		LIMIT ?`, args
}

// formatSessionRow renders one row for CLI output. Tab-separated so
// downstream column -t / awk / cut behave. First prompt is truncated
// and newlines flattened; empty-session sentinels render as "-".
func formatSessionRow(id string, startedMs, endedMs sql.NullInt64, eventCount int, cwd, firstPrompt sql.NullString) string {
	sess := id
	if len(sess) > 8 {
		sess = sess[:8]
	}
	return fmt.Sprintf(
		"%s\t%s\t%s\t%d\t%s\t%s",
		sess,
		formatTsNullable(startedMs),
		formatTsNullable(endedMs),
		eventCount,
		nullStringOrDash(cwd),
		truncatePrompt(nullStringOrDash(firstPrompt)),
	)
}

func formatTsNullable(n sql.NullInt64) string {
	if !n.Valid {
		return "-"
	}
	return time.UnixMilli(n.Int64).UTC().Format("2006-01-02T15:04:05Z")
}

func nullStringOrDash(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	return s.String
}

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
