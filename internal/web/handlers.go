package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/pkg/events"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
)

// sessionsListLimit is how many sessions /  loads. Aichronicles is a
// personal-use store; tens of thousands of sessions are unlikely.
// 100 fits in a scrollable table without paging UI in v1.
const sessionsListLimit = 100

// sessionsHandler renders the sessions list page at /. Returns the
// most-recently-active sessions first, with a `summary` badge on
// rows that already have a cached LLM summary in llm_outputs.
//
// Faceted-filter query params (all optional, all combine with AND):
//   - agent          — exact source_agent (claude-code | gemini-cli)
//   - project        — project-root cwd from /projects click-through
//   - tool           — exact tool_name on some event in the session
//   - skill          — sessions whose events loaded the named skill
//   - file           — substring match on a file_path extraction
//   - with-failures  — sessions with ≥1 tool_failure event (truthy values)
//
// Each active filter renders as a removable chip in the template;
// removing one preserves the others.
func (s *Server) sessionsHandler(w http.ResponseWriter, r *http.Request) {
	filters := readSessionListFilters(r)
	rows, err := loadSessionsForList(r.Context(), s.store, sessionsListLimit, filters)
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
	if filters.Agent != "" {
		title = "Sessions · " + filters.Agent
	}
	if filters.Project != "" {
		title = "Sessions · " + filters.Project
	}
	page := SessionsPage{
		Title:              title,
		Sessions:           rows,
		Agents:             agents,
		ActiveAgent:        filters.Agent,
		ActiveProject:      filters.Project,
		ActiveTool:         filters.Tool,
		ActiveSkill:        filters.Skill,
		ActiveFile:         filters.File,
		ActiveWithFailures: filters.WithFailures,
		ActiveNoSummary:    filters.NoSummary,
		FilterChips:        buildSessionListChips("/", filters),
	}
	s.render(w, r, "sessions", page)
}

// sessionListFilters collects every faceted-filter param the
// sessions list understands. Single struct so plumbing it through
// the handler / loader / chip-builder doesn't fan out into a
// long argument list every time we add a facet.
type sessionListFilters struct {
	Agent        string
	Project      string
	Tool         string
	Skill        string
	File         string
	WithFailures bool
	// NoSummary narrows to sessions that DON'T yet have an
	// llm_outputs row of kind=summary. Pairs with the
	// "(no summary yet)" muted placeholder on the row preview —
	// the chip is the way to find every row sporting that
	// placeholder so the user can queue them for `summaries fill`.
	NoSummary bool
}

// readSessionListFilters parses the query string into the filter
// struct. TrimSpace on every string so trailing whitespace from a
// link-builder mistake doesn't accidentally narrow the result set
// to zero rows; with-failures accepts the usual truthy values
// ("1", "true", "yes") to keep the URL hand-typeable.
func readSessionListFilters(r *http.Request) sessionListFilters {
	q := r.URL.Query()
	return sessionListFilters{
		Agent:        strings.TrimSpace(q.Get("agent")),
		Project:      strings.TrimSpace(q.Get("project")),
		Tool:         strings.TrimSpace(q.Get("tool")),
		Skill:        strings.TrimSpace(q.Get("skill")),
		File:         strings.TrimSpace(q.Get("file")),
		WithFailures: parseTruthy(q.Get("with-failures")),
		NoSummary:    parseTruthy(q.Get("no-summary")),
	}
}

// parseTruthy accepts "1", "true", "yes", "on" (case-insensitive)
// as true; everything else (including the empty string) is false.
// Mirrors what most form checkboxes serialize as.
func parseTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// buildSessionListChips renders one FilterChip per ACTIVE filter,
// in a stable display order. Each chip's HrefRemove drops only that
// one filter from the URL while preserving the others, so the user
// can pull one constraint off without losing the rest of their
// drill-down.
//
// basePath is "/" for the sessions list and "/search" for the
// search page (where the chips live alongside the search form).
func buildSessionListChips(basePath string, f sessionListFilters) []FilterChip {
	var chips []FilterChip
	add := func(label, removeKey string) {
		clone := f
		switch removeKey {
		case "agent":
			clone.Agent = ""
		case "project":
			clone.Project = ""
		case "tool":
			clone.Tool = ""
		case "skill":
			clone.Skill = ""
		case "file":
			clone.File = ""
		case "with-failures":
			clone.WithFailures = false
		case "no-summary":
			clone.NoSummary = false
		}
		chips = append(chips, FilterChip{
			Label:      label,
			HrefRemove: filtersToURL(basePath, clone),
		})
	}
	if f.Project != "" {
		add("project: "+f.Project, "project")
	}
	if f.Agent != "" {
		add("agent: "+f.Agent, "agent")
	}
	if f.Tool != "" {
		add("tool: "+f.Tool, "tool")
	}
	if f.Skill != "" {
		add("skill: "+f.Skill, "skill")
	}
	if f.File != "" {
		add("file: "+f.File, "file")
	}
	if f.WithFailures {
		add("with failures", "with-failures")
	}
	if f.NoSummary {
		add("no summary", "no-summary")
	}
	return chips
}

// filtersToURL re-serialises the filter struct back into a URL.
// Empty / falsy filters are omitted entirely so the resulting URL
// stays minimal — `/?agent=claude-code` rather than
// `/?agent=claude-code&tool=&skill=&file=&with-failures=`. URL-
// encodes every value via url.Values.
func filtersToURL(basePath string, f sessionListFilters) string {
	v := url.Values{}
	if f.Agent != "" {
		v.Set("agent", f.Agent)
	}
	if f.Project != "" {
		v.Set("project", f.Project)
	}
	if f.Tool != "" {
		v.Set("tool", f.Tool)
	}
	if f.Skill != "" {
		v.Set("skill", f.Skill)
	}
	if f.File != "" {
		v.Set("file", f.File)
	}
	if f.WithFailures {
		v.Set("with-failures", "1")
	}
	if f.NoSummary {
		v.Set("no-summary", "1")
	}
	if len(v) == 0 {
		return basePath
	}
	return basePath + "?" + v.Encode()
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
// All filters in the struct are optional; multiple combine with AND.
// Empty / false values are skipped. Tool / skill / file / failures
// are session-level: a session matches if ANY of its events satisfies
// the predicate.
func loadSessionsForList(ctx context.Context, st *store.Store, limit int, f sessionListFilters) ([]SessionRow, error) {
	q := `
		SELECT s.id,
		       s.started_at_ms,
		       s.ended_at_ms,
		       s.event_count,
		       s.cwd,
		       s.source_agent,
		       s.first_prompt_text AS first_prompt
		  FROM sessions s`
	var conds []string
	args := []any{}
	if f.Agent != "" {
		conds = append(conds, "s.source_agent = ?")
		args = append(args, f.Agent)
	}
	if f.Project != "" {
		// Match sessions whose latest cwd is the project root or
		// any descendant. Approximation: a session that started
		// in /proj but cd'd to /elsewhere mid-way will still be
		// indexed by its latest cwd, which is the trade-off
		// sessions.cwd makes today. Good enough for a click-
		// through filter; the /projects page uses start cwd for
		// its rollup.
		conds = append(conds, "(s.cwd = ? OR s.cwd LIKE ?)")
		args = append(args, f.Project, f.Project+"/%")
	}
	if f.Tool != "" {
		// EXISTS over events (covered by idx_events_session_ts and
		// the partial idx for tool-bearing rows). Cheap because
		// SQLite short-circuits on the first hit.
		conds = append(conds, `EXISTS (
			SELECT 1 FROM events e
			 WHERE e.session_id = s.id AND e.tool_name = ?
		)`)
		args = append(args, f.Tool)
	}
	if f.Skill != "" {
		// extractions kind=skill_load value=<name> is the canonical
		// signal — see pkg/events/SkillLoadExtractor.
		conds = append(conds, `EXISTS (
			SELECT 1 FROM extractions x
			 WHERE x.session_id = s.id AND x.kind = ? AND x.value = ?
		)`)
		args = append(args, events.ExtractionKindSkillLoad, f.Skill)
	}
	if f.File != "" {
		// Substring LIKE so a partial path ("migrate.go",
		// "internal/store") matches. Same shape the CLI's
		// FilePathSubstring filter uses.
		conds = append(conds, `EXISTS (
			SELECT 1 FROM extractions x
			 WHERE x.session_id = s.id AND x.kind = ? AND x.value LIKE ?
		)`)
		args = append(args, events.ExtractionKindFilePath, "%"+f.File+"%")
	}
	if f.WithFailures {
		conds = append(conds, `EXISTS (
			SELECT 1 FROM events e
			 WHERE e.session_id = s.id AND e.kind = ?
		)`)
		args = append(args, events.KindToolFailure)
	}
	if f.NoSummary {
		// NOT EXISTS over llm_outputs(kind=summary). NOT EXISTS
		// short-circuits on the first match, so this stays cheap
		// even on a large llm_outputs table — the per-session
		// scan stops as soon as one summary row is found.
		conds = append(conds, `NOT EXISTS (
			SELECT 1 FROM llm_outputs lo
			 WHERE lo.session_id = s.id AND lo.kind = ?
		)`)
		args = append(args, string(store.LLMKindSummary))
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY " + store.EffectiveTsExpr + " DESC LIMIT ?"
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
		out[i].Preview, out[i].PreviewKind = pickRowPreview(out[i].SummaryTopic, out[i].FirstPrompt)

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

// pickRowPreview chooses the row's primary description text +
// styling hint via internal/preview so the web row renderer, MCP
// snippet builder, and CLI completion picker all agree on the
// priority (topic > substantive prompt > muted placeholder).
func pickRowPreview(summaryTopic, firstPrompt string) (text, kind string) {
	t, k := preview.Pick(summaryTopic, firstPrompt)
	return t, string(k)
}

// isSubstantivePrompt is a thin wrapper over preview.IsSubstantivePrompt
// kept for in-package call sites (template funcs, tests) that
// already use the local name.
func isSubstantivePrompt(s string) bool {
	return preview.IsSubstantivePrompt(strings.TrimSpace(s))
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
// Thin wrapper over internal/timefmt — kept here so handlers
// stay readable, and so the web's "future is also -" override
// (versus timefmt's "future?") lands in one place.
func relativeTime(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	r := timefmt.Relative(ms, now)
	if r == "future?" {
		return "-"
	}
	return r
}

// orDash returns the string content of a sql.NullString, or "-"
// when NULL or empty. Thin wrapper over internal/nullable so the
// display contract stays identical across surfaces.
func orDash(s sql.NullString) string {
	return nullable.OrDash(s)
}

// truncatePreview flattens whitespace and rune-caps a prompt
// preview for use in a single table cell. Wraps internal/preview
// so the cap matches MCP's oneLineSnippet and the CLI snippet
// renderers — one number to tweak when the layout changes.
func truncatePreview(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	return preview.OneLine(s.String)
}

// truncatePreviewString is the plain-string variant of
// truncatePreview, for callers that already have a string and
// want the same flatten-and-cap rendering.
func truncatePreviewString(s string) string {
	if s == "" {
		return "-"
	}
	return preview.OneLine(s)
}
