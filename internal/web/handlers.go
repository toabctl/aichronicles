package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// previewMaxRunes caps how much of a prompt or content blob we
// render in a list-row preview. Generous enough to surface real
// intent ("how do I parse jsonl…"), tight enough to keep table
// rows on one terminal-style line.
const previewMaxRunes = 120

// sessionsListLimit is how many sessions /  loads. Aichronicles is a
// personal-use store; tens of thousands of sessions are unlikely.
// 100 fits in a scrollable table without paging UI in v1.
const sessionsListLimit = 100

// sessionsHandler renders the sessions list page at /. Returns the
// most-recently-active sessions first, with a `summary` badge on
// rows that already have a cached LLM summary in llm_outputs.
func (s *Server) sessionsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := loadSessionsForList(r.Context(), s.store, sessionsListLimit)
	if err != nil {
		s.log.Error("sessionsHandler: load", "err", err)
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	page := SessionsPage{Title: "Sessions", Sessions: rows}
	s.render(w, r, "sessions", page)
}

// loadSessionsForList runs the read query backing the sessions page.
// One SQL roundtrip pulls the session rows + first-prompt preview;
// a second indexed query fetches each session's cached summary so
// the row can show the model-attributed topic alongside the prompt.
func loadSessionsForList(ctx context.Context, st *store.Store, limit int) ([]SessionRow, error) {
	const q = `
		SELECT s.id,
		       s.started_at_ms,
		       s.ended_at_ms,
		       s.event_count,
		       s.cwd,
		       (SELECT content_text FROM events
		          WHERE session_id = s.id AND kind = 'user_prompt'
		          ORDER BY ts_source_ms ASC LIMIT 1) AS first_prompt
		  FROM sessions s
		 ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
		 LIMIT ?`

	rows, err := st.DB().QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := time.Now()
	var out []SessionRow
	var ids []string
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
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		out = append(out, SessionRow{
			ID:           id,
			ShortID:      shortID(id),
			LastActivity: relativeTime(effectiveTs(startedMs, endedMs), now),
			EventCount:   eventCount,
			Cwd:          orDash(cwd),
			FirstPrompt:  truncatePreview(firstPrompt),
		})
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session rows: %w", err)
	}

	summaries, err := store.LoadSummariesIndexedByID(ctx, st.DB(), ids)
	if err != nil {
		return nil, fmt.Errorf("load summaries: %w", err)
	}
	latestEvents, err := store.LoadLatestEventsIndexedByID(ctx, st.DB(), ids)
	if err != nil {
		return nil, fmt.Errorf("load latest events: %w", err)
	}

	for i := range out {
		if summary, ok := summaries[out[i].ID]; ok {
			out[i].HasSummary = true
			out[i].SummaryTopic = parseSummaryTopic(summary.Body)
		}

		// Status dot: ended / active / idle. Driven by the latest
		// event — its kind tells us whether the session has
		// formally ended (session_end), its timestamp tells us
		// whether activity is fresh enough to count as "active".
		// Sessions without any events fall to "idle".
		var latestPtr *store.LiveEvent
		if e, ok := latestEvents[out[i].ID]; ok {
			latestPtr = &e
			out[i].LatestEventHTML = template.HTML(renderLatestEventCell(e))
		}
		status, title := sessionStatus(latestPtr, now)
		out[i].StatusDotHTML = template.HTML(renderStatusDot(out[i].ID, status, title, false))
	}
	return out, nil
}

// parseSummaryTopic extracts the `topic` field from a cached
// summary body. Returns empty string when the body is malformed
// JSON or the topic is missing — the row still renders the badge,
// just without the inline topic line. Scoped narrow on purpose:
// a one-liner per row doesn't need the rest of SummaryResult.
func parseSummaryTopic(body string) string {
	var parsed prompts.SummaryResult
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return ""
	}
	return parsed.Topic
}

// shortID returns the 8-char preview the CLI uses everywhere
// session ids appear.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// effectiveTs picks ended_at_ms if set, else started_at_ms, else 0.
// Same expression the schema's idx_sessions_effective_ts index is
// built on.
func effectiveTs(startedMs, endedMs sql.NullInt64) int64 {
	if endedMs.Valid {
		return endedMs.Int64
	}
	if startedMs.Valid {
		return startedMs.Int64
	}
	return 0
}

// relativeTime renders an epoch-millis timestamp as "2h ago" /
// "3d ago" relative to now. Zero / future times render as "-".
// We don't try to be cute about pluralisation — list cells need
// to be uniform width.
func relativeTime(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 0:
		return "-"
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

// orDash returns the string content of a sql.NullString, or "-"
// when NULL or empty. Mirrors the CLI's display contract.
func orDash(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	return s.String
}

// truncatePreview flattens whitespace and rune-caps a prompt
// preview for use in a single table cell.
func truncatePreview(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	text := s.String
	for _, r := range "\n\r\t" {
		text = strings.ReplaceAll(text, string(r), " ")
	}
	runes := []rune(text)
	if len(runes) <= previewMaxRunes {
		return text
	}
	return string(runes[:previewMaxRunes]) + "…"
}
