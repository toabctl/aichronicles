package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
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
		limit    int
		cwd      string
		agent    string
		since    time.Duration
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List sessions in the store, most-recently-ended first",
		Long: "One row per session, columns:\n\n" +
			"  SESSION  STARTED  ENDED  EVENTS  CWD  FIRST_PROMPT\n\n" +
			"On a TTY columns are aligned for reading; when piped or\n" +
			"redirected they emit as tab-separated values for awk/cut.\n" +
			"Pass --format=json for a structured payload suitable for jq.\n" +
			"Filters stack.",
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

			opts := SessionsOptions{
				Cwd:    cwd,
				Agent:  agent,
				Limit:  limit,
				Format: format,
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
	cmd.Flags().DurationVar(&since, "since", 0, "only sessions whose ended_at is within this duration (search/audit filter on per-event ts_source)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// SessionsOptions is the filter set for listing sessions. Exported so
// tests and MCP wiring can drive the same code path.
type SessionsOptions struct {
	Cwd     string
	Agent   string
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

// RunListSessions queries the store and writes one row per session
// to out. Filters stack; ordering is always most-recently-ended first.
//
// Format=table (default) renders aligned columns + tab-separated
// underneath for awk/cut. Format=json emits a JSON array of
// SessionRowJSON values for jq pipelines. Empty result sets print a
// "(no sessions matched)" line in table mode and an empty array in
// JSON mode so consumers always see well-formed output.
func RunListSessions(s *store.Store, opts SessionsOptions, out io.Writer) error {
	sqlText, args := buildSessionsSQL(opts)
	rows, err := s.DB().Query(sqlText, args...)
	if err != nil {
		return fmt.Errorf("sessions query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rawRow struct {
		id          string
		startedMs   sql.NullInt64
		endedMs     sql.NullInt64
		eventCount  int
		cwd         sql.NullString
		firstPrompt sql.NullString
	}
	var collected []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.id, &r.startedMs, &r.endedMs, &r.eventCount, &r.cwd, &r.firstPrompt); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if opts.Format == FormatJSON {
		payload := make([]SessionRowJSON, 0, len(collected))
		for _, r := range collected {
			payload = append(payload, SessionRowJSON{
				SessionID:   r.id,
				StartedAtMs: nullableInt64(r.startedMs),
				EndedAtMs:   nullableInt64(r.endedMs),
				EventCount:  r.eventCount,
				Cwd:         nullableString(r.cwd),
				FirstPrompt: nullableString(r.firstPrompt),
			})
		}
		return emitJSON(out, payload)
	}

	if len(collected) == 0 {
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
	for _, r := range collected {
		_, _ = fmt.Fprintln(tw, formatSessionRow(r.id, r.startedMs, r.endedMs, r.eventCount, r.cwd, r.firstPrompt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = io.Copy(out, &buf)
	return err
}

// nullableInt64 lifts sql.NullInt64 into *int64 so JSON output renders
// missing timestamps as `null` rather than `0`.
func nullableInt64(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// nullableString lifts sql.NullString into *string. Empty-but-valid is
// preserved as "" (distinct from null) — the schema treats empty cwd
// as legitimately absent of a working directory rather than missing.
func nullableString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
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
	return formatTimeForUser(n.Int64, time.Now())
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
