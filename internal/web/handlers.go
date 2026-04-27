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
//
// Optional `?agent=<slug>` query param narrows the list to one
// source agent (claude-code | gemini-cli). The filter shows up as
// a removable chip in the rendered template; an unknown / empty
// slug falls back to the unfiltered view rather than 404'ing.
func (s *Server) sessionsHandler(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	rows, err := loadSessionsForList(r.Context(), s.store, sessionsListLimit, agent, project)
	if err != nil {
		s.log.Error("sessionsHandler: load", "err", err)
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	agents, err := loadDistinctSourceAgents(r.Context(), s.store)
	if err != nil {
		s.log.Error("sessionsHandler: load agents", "err", err)
		// Non-fatal — the list still renders, just without filter chips.
		agents = nil
	}
	title := "Sessions"
	if agent != "" {
		title = "Sessions · " + agent
	}
	if project != "" {
		title = "Sessions · " + project
	}
	page := SessionsPage{
		Title:         title,
		Sessions:      rows,
		Agents:        agents,
		ActiveAgent:   agent,
		ActiveProject: project,
	}
	s.render(w, r, "sessions", page)
}

// loadDistinctSourceAgents returns the set of source_agent slugs
// present in the sessions table, sorted alphabetically. Used to
// render filter chips on the sessions list — we don't hardcode
// "claude-code | gemini-cli" because the list grows whenever a
// new agent's importer / setup command lands.
func loadDistinctSourceAgents(ctx context.Context, st *store.Store) ([]string, error) {
	rows, err := st.DB().QueryContext(ctx,
		`SELECT DISTINCT source_agent FROM sessions ORDER BY source_agent ASC`)
	if err != nil {
		return nil, fmt.Errorf("query distinct source_agent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadSessionsForList runs the read query backing the sessions page.
// One SQL roundtrip pulls the session rows + first-prompt preview;
// a second indexed query fetches each session's cached summary so
// the row can show the model-attributed topic alongside the prompt.
//
// agentFilter, when non-empty, narrows to sessions where
// source_agent = <filter>. Empty value lists every agent (the
// default).
func loadSessionsForList(ctx context.Context, st *store.Store, limit int, agentFilter, projectFilter string) ([]SessionRow, error) {
	q := `
		SELECT s.id,
		       s.started_at_ms,
		       s.ended_at_ms,
		       s.event_count,
		       s.cwd,
		       s.source_agent,
		       (SELECT content_text FROM events
		          WHERE session_id = s.id AND kind = 'user_prompt'
		          ORDER BY ts_source_ms ASC LIMIT 1) AS first_prompt
		  FROM sessions s`
	var conds []string
	args := []any{}
	if agentFilter != "" {
		conds = append(conds, "s.source_agent = ?")
		args = append(args, agentFilter)
	}
	if projectFilter != "" {
		// Match sessions whose latest cwd is the project root or
		// any descendant. Approximation: a session that started
		// in /proj but cd'd to /elsewhere mid-way will still be
		// indexed by its latest cwd, which is the trade-off
		// sessions.cwd makes today. Good enough for a click-
		// through filter; the /projects page uses start cwd for
		// its rollup.
		conds = append(conds, "(s.cwd = ? OR s.cwd LIKE ?)")
		args = append(args, projectFilter, projectFilter+"/%")
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC LIMIT ?"
	args = append(args, limit)

	rows, err := st.DB().QueryContext(ctx, q, args...)
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
			sourceAgent string
			firstPrompt sql.NullString
		)
		if err := rows.Scan(&id, &startedMs, &endedMs, &eventCount, &cwd, &sourceAgent, &firstPrompt); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		out = append(out, SessionRow{
			ID:           id,
			ShortID:      shortID(id),
			LastActivity: relativeTime(effectiveTs(startedMs, endedMs), now),
			EventCount:   eventCount,
			Cwd:          orDash(cwd),
			SourceAgent:  sourceAgent,
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
			out[i].SummaryTopic, out[i].SummaryTooltip = parseSummaryForBadge(summary.Body)
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

// parseSummaryForBadge extracts the topic AND a multi-line tooltip
// from a cached summary body. The tooltip carries the topic plus
// the "what was done" bullets so a hover on the badge surfaces the
// whole gist of the session without a click. Returns empty strings
// when the body is malformed JSON — the row still renders the
// badge, just without the inline topic line and without a tooltip.
//
// The tooltip is plain text with newlines so the browser's native
// title= attribute renders it as a multi-line popover; no CSS or
// markup needed beyond the title= itself.
func parseSummaryForBadge(body string) (topic, tooltip string) {
	var parsed prompts.SummaryResult
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", ""
	}
	topic = parsed.Topic

	var b strings.Builder
	if topic != "" {
		b.WriteString(topic)
	}
	if len(parsed.WhatWasDone) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("What was done:")
		// Cap at 5 bullets so a runaway summary doesn't produce
		// a tooltip that overflows the screen. Most summaries
		// cap at 8 by their own schema; 5 is enough to scan.
		const maxBullets = 5
		for i, d := range parsed.WhatWasDone {
			if i >= maxBullets {
				b.WriteString("\n• …")
				break
			}
			b.WriteString("\n• ")
			b.WriteString(d)
		}
	}
	if len(parsed.Unresolved) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Unresolved:")
		const maxBullets = 3
		for i, u := range parsed.Unresolved {
			if i >= maxBullets {
				b.WriteString("\n• …")
				break
			}
			b.WriteString("\n• ")
			b.WriteString(u)
		}
	}
	return topic, b.String()
}

// parseSummaryTopic preserves the older two-return-value-free
// helper signature for callers that only want the topic. Defined
// in terms of parseSummaryForBadge so the parsing logic isn't
// duplicated.
func parseSummaryTopic(body string) string {
	t, _ := parseSummaryForBadge(body)
	return t
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
