package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"

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
	rows, err := loadSessionsForList(r.Context(), s, sessionsListLimit, filters)
	if err != nil {
		s.internalError(w, "sessionsHandler: load", "could not load sessions", err)
		return
	}
	agents, err := s.api.SourceAgents(r.Context())
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

// loadSessionsForList runs the read flow backing the sessions page.
// Three apiclient round-trips: (1) the faceted session list, (2)
// batch summary lookup for the badge tooltip + topic, (3) batch
// latest-event lookup for the status dot + Latest column.
//
// All filters in the struct are optional; multiple combine with AND
// server-side. Empty / false values are skipped.
func loadSessionsForList(ctx context.Context, s *Server, limit int, f sessionListFilters) ([]SessionRow, error) {
	digests, err := s.api.Sessions(ctx, wire.SessionListRequest{
		Limit: limit,
		// SinceMs=1 means "any session, no time cutoff." The api
		// applies a 30-day default when since_ms is unset; the web
		// list deliberately shows the full corpus capped at limit,
		// so we pass a tiny epoch value to disable the cutoff
		// without changing the wire contract.
		SinceMs:           1,
		SourceAgent:       f.Agent,
		Project:           f.Project,
		ToolName:          f.Tool,
		SkillName:         f.Skill,
		FilePathSubstring: f.File,
		WithFailures:      f.WithFailures,
		WithoutSummary:    f.NoSummary,
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	now := time.Now()
	out := make([]SessionRow, 0, len(digests.Sessions))
	ids := make([]string, 0, len(digests.Sessions))
	for _, d := range digests.Sessions {
		// Resume one-liners use start_cwd because `claude --resume`
		// indexes transcripts by session-start cwd (see the longer
		// rationale on buildResumeCommandPtr). Fall back to the
		// row's latest cwd when start_cwd is nil — matches the
		// session-detail handler's same fallback so the two pages
		// always agree on the rendered command.
		resumeCwd := d.StartCwd
		if resumeCwd == nil {
			resumeCwd = d.Cwd
		}
		out = append(out, SessionRow{
			ID:                     d.ID,
			ShortID:                preview.ShortID(d.ID),
			LastActivity:           relativeTime(effectiveTsPtr(d.StartedAtMs, d.EndedAtMs), now),
			EventCount:             d.EventCount,
			Cwd:                    orDashPtr(d.Cwd),
			SourceAgent:            d.SourceAgent,
			FirstPrompt:            truncatePreviewString(derefOr(d.FirstPrompt, "")),
			ResumeCommand:          buildResumeCommandPtr(d.SourceAgent, d.SourceSessionID, resumeCwd),
			ResumeCommandDangerous: buildResumeCommandDangerousPtr(d.SourceAgent, d.SourceSessionID, resumeCwd),
		})
		ids = append(ids, d.ID)
	}

	// SummariesBatch and EventsLatestBatch are independent —
	// dispatch in parallel so the page's latency floor is one RTT
	// (the longer call) instead of two stacked sequentially.
	// errgroup.WithContext cancels the sibling call as soon as
	// either errors out, and guarantees both goroutines have
	// returned before Wait() releases — no leak on caller cancel.
	var (
		summaries    map[string]wire.LLMOutput
		latestEvents map[string]wire.Event
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		summaries, err = s.api.SummariesBatch(gctx, ids)
		if err != nil {
			return fmt.Errorf("load summaries: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var err error
		latestEvents, err = s.api.EventsLatestBatch(gctx, ids)
		if err != nil {
			return fmt.Errorf("load latest events: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
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
		var latestPtr *wire.Event
		if e, ok := latestEvents[out[i].ID]; ok {
			latestPtr = &e
			out[i].LatestEventHTML = template.HTML(renderLatestEventCell(e))
		}
		status, title := sessionStatus(latestPtr, now)
		out[i].StatusDotHTML = template.HTML(renderStatusDot(out[i].ID, status, title, false))
	}
	return out, nil
}

// effectiveTsPtr is the *int64 sibling of effectiveTs: returns
// *ended when set, else *started, else 0.
func effectiveTsPtr(started, ended *int64) int64 {
	if ended != nil && *ended != 0 {
		return *ended
	}
	if started != nil && *started != 0 {
		return *started
	}
	return 0
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

// orDashPtr returns *s when non-empty, else "-". Display contract
// shared by every table cell that surfaces a possibly-null wire
// column.
func orDashPtr(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// truncatePreviewString flattens whitespace and rune-caps a prompt
// preview for use in a single table cell. Wraps internal/preview so
// the cap matches MCP's oneLineSnippet and the CLI snippet
// renderers — one number to tweak when the layout changes.
func truncatePreviewString(s string) string {
	if s == "" {
		return "-"
	}
	return preview.OneLine(s)
}

// derefOr returns *s or fallback if s is nil. Tiny helper for the
// many sites that consume the store's pointer-typed optional
// fields (arch_review_2026_05_13 MEDIUM #10) and need a string in
// hand for templates.
func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}
