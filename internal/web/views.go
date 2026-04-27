package web

import (
	"html/template"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// Page is the common envelope every route's view data sits inside.
// Title flows into the <title> tag in base.html.
type Page struct {
	Title string
	Data  any
}

// SessionsPage is the data shape the sessions list template
// consumes. Keeping the rendering shape (this struct) separate
// from the storage shape (rows from store) lets us format
// timestamps, truncate prompts, and compute display-only fields
// (ShortID, LastActivity) once per request without pushing
// presentation logic into the SQL layer.
type SessionsPage struct {
	Title    string
	Sessions []SessionRow
	// Agents is the list of distinct source_agent slugs present in
	// the store. The template renders one filter chip per slug
	// linking to ?agent=<slug>; nil/empty hides the chip row.
	Agents []string
	// ActiveAgent is the currently-applied filter (matches one of
	// Agents when set). Empty value means "no filter."
	ActiveAgent string
	// ActiveProject is the project-root filter from /projects
	// click-through. Rendered as a removable chip. Empty when
	// no project filter is set.
	ActiveProject string

	// Faceted-search filters mirrored from #96's CLI surface. Each,
	// when non-empty / true, narrows the candidate sessions and
	// renders as a removable chip in the template. Multiple filters
	// combine with AND.
	ActiveTool         string // events with tool_name = ?
	ActiveSkill        string // sessions whose events loaded this skill
	ActiveFile         string // sessions whose events touched a file matching this substring
	ActiveWithFailures bool   // sessions with ≥1 tool_failure event
	ActiveNoSummary    bool   // sessions with NO llm_outputs(kind=summary) row

	// FilterChips is a pre-rendered list of removable filter chips
	// for the template. Each chip carries the label to display and
	// the href that drops only this one filter (preserving the
	// others). Built once in the handler so the template stays
	// free of URL-construction logic.
	FilterChips []FilterChip
}

// FilterChip is one removable filter on the sessions list (and
// /search). Label is what the chip shows; HrefRemove is the URL
// to navigate to with this filter cleared and all others kept.
type FilterChip struct {
	Label      string
	HrefRemove string
}

// SessionRow is one row in the sessions list. Display strings
// are pre-rendered server-side so the template stays free of
// formatting helpers — easier to test and easier to keep tidy.
type SessionRow struct {
	ID             string // full UUID — used in /sessions/{id} hrefs
	ShortID        string // 8-char preview — what the user sees in the row
	LastActivity   string // human-friendly relative time ("2h ago")
	EventCount     int
	Cwd            string // working directory at last event, or "-"
	SourceAgent    string // claude-code | gemini-cli — drives the agent badge
	FirstPrompt    string // truncated first user_prompt content_text
	HasSummary     bool   // an llm_outputs(kind='summary') row exists for this session
	SummaryTopic   string // parsed `topic` field from the cached summary; empty when none
	SummaryTooltip string // multi-line plain-text tooltip for the summary badge — topic + "what was done" bullets, native browser tooltip

	// Preview is the row's primary description. SummaryTopic when
	// available (the model's actual distillation of the session);
	// otherwise the first user_prompt — but only if it's substantive
	// (≥30 chars after trim, not a follow-up like "go ahead" /
	// "/loop"). When neither is useful, PreviewKind is "muted" and
	// Preview is a placeholder ("no summary yet"), so the row
	// doesn't lie about a 3-hour session by showing "yes please" as
	// its primary text.
	Preview     string
	PreviewKind string // "topic" | "prompt" | "muted"

	// StatusDotHTML is the pre-rendered <span class="status …">●</span>
	// for the activity dot. Pre-rendered so the same Go-side renderer
	// powers both initial server-side render and SSE-driven OOB
	// updates; the template just embeds it via {{.StatusDotHTML}}.
	StatusDotHTML template.HTML

	// LatestEventHTML is the pre-rendered inner content of the
	// "Latest event" cell (ts + kind badge + snippet). Empty when
	// the session has no events yet — the template falls through
	// to a muted placeholder in that case.
	LatestEventHTML template.HTML
}

// SessionDetail is the data shape the session detail template
// consumes. Summary is nil when no cached LLM summary exists for
// this session.
type SessionDetail struct {
	Title      string
	ID         string // full UUID — our derived id (UUIDv5)
	ShortID    string
	Cwd        string
	StartedAt  string // absolute UTC, "2026-04-24T12:00:00Z"
	EndedAt    string // absolute UTC, or "(active)" if no ended_at_ms yet
	EventCount int
	Summary    *SessionSummary // nil if no cached summary
	Events     []EventRow

	// Resume hooks: SourceAgent + SourceSessionID identify the
	// session to the originating agent (claude-code, codex). The
	// derived ID we display elsewhere is OUR id (UUIDv5 of agent
	// + source id) and is NOT what `claude --resume` accepts.
	// ResumeCommand is the pre-rendered shell one-liner the
	// "Resume" button copies — empty when SourceAgent is unknown.
	SourceAgent     string
	SourceSessionID string
	ResumeCommand   string
}

// SessionSummary is the rendered shape of an llm_outputs row of
// kind='summary'. Mirrors prompts.SummaryResult one-for-one but
// kept in this package so the template doesn't reach into
// pkg/llm/prompts directly.
type SessionSummary struct {
	Topic       string
	WhatWasDone []string
	Unresolved  []string
	KeyFiles    []string
	Links       []SummaryLink
	Model       string
	GeneratedAt string // human-readable
}

// SummaryLink pairs a URL with its model-attributed `used_for`
// annotation.
type SummaryLink struct {
	URL     string
	UsedFor string
}

// EventRow is one row in the session detail's event timeline.
// The full event_id is intentionally omitted — list rows don't
// need it, and surfacing it would just clutter the UI.
type EventRow struct {
	When    string // relative time
	Kind    string
	Role    string
	Tool    string // empty if not a tool event
	Snippet string // truncated content_text preview
}

// ProjectsPage drives /projects: one row per project root, with
// session/event counts and last-activity timestamps. Project
// roots are derived by rolling up start cwds to the nearest
// .claude / .git / go.mod ancestor.
type ProjectsPage struct {
	Title    string
	Days     int
	Empty    bool
	Projects []ProjectRow
}

// ProjectRow is one row in the projects list. SortKey is exposed
// only for in-package sort stability; the template doesn't render
// it.
type ProjectRow struct {
	Root         string // absolute path of the project root
	Sessions     int
	Events       int
	LastActivity string // relative time ("2h ago")
	DistinctCwds int    // how many distinct start cwds rolled into this root
	SortKey      int64  // last_activity_ms for stable sort
}

// SkillsPage is the data shape /skills consumes. Three sections:
// installed (SKILL.md files on disk), invoked (skill_load
// extractions ranked by frequency), stale (skills whose loads
// correlate with tool_failure events).
type SkillsPage struct {
	Title     string
	Days      int
	Installed []prompts.InstalledSkill
	Invoked   []prompts.InvokedSkill
	Stale     []StaleSkillRow
}

// StaleSkillRow is one row in the stale-candidates table. Pre-
// computed Rate (0–100 integer percent) and Examples (with
// short ids ready for /sessions/ links) keep the template free
// of arithmetic / string-slicing.
type StaleSkillRow struct {
	Name       string
	TotalLoads int
	StaleLoads int
	Rate       int // percent, 0–100
	Examples   []StaleExample
}

// StaleExample is one example session listed under a stale skill.
type StaleExample struct {
	SessionID string // full id for /sessions/<id>
	ShortID   string // 8-char preview
}

// InsightsPage is the data shape the /insights template consumes.
// All formatting (relative times, percentages, bar widths) is
// pre-computed in buildInsightsPage so the template stays free
// of helpers.
type InsightsPage struct {
	Title          string
	Days           int
	Since          string // "2026-04-01"
	Until          string
	Empty          bool
	Overview       store.InsightsOverview
	TopTools       []InsightsToolRow
	TopSkills      []InsightsSkillRow
	ActivityByHour []InsightsHourRow // 24 entries
	TopSessions    []InsightsSessionRow
}

// InsightsToolRow is one row of the "top tools" table.
type InsightsToolRow struct {
	ToolName string
	Count    int
	Percent  float64 // 0–100
}

// InsightsSkillRow is one row of the "top skills" table.
type InsightsSkillRow struct {
	Name     string
	Count    int
	LastUsed string // relative ("2h ago")
}

// InsightsHourRow is one bar in the activity-by-hour histogram.
// Width is a 0–100 percentage of the busiest hour's count, so
// the template can render `style="width: {{.Width}}%"` directly.
type InsightsHourRow struct {
	Hour  int
	Count int
	Width float64
}

// InsightsSessionRow is one row in the "top sessions" table.
type InsightsSessionRow struct {
	SessionID   string
	ShortID     string
	EventCount  int
	Cwd         string
	Started     string
	FirstPrompt string
}

// DigestsPage drives /digests: cards of cached weekly reflection
// artefacts (llm_outputs.kind = reflect_weekly), newest first.
// One card per persisted digest; each card carries the analysed
// week, the workflow_change one-liner, and collapsible sections
// for task_types and frictions with linked evidence.
type DigestsPage struct {
	Title   string
	Limit   int
	Digests []DigestCard
}

// DigestCard is one rendered digest. Period / WorkflowChange /
// TaskTypes / Frictions land when the persisted body parses;
// RawBody holds the unparsed JSON when it doesn't, so a single
// bad artifact doesn't break the whole page.
type DigestCard struct {
	ID             int64
	Model          string
	Generated      string // relative ("3d ago")
	GeneratedAt    string // absolute UTC
	Period         string // "Apr 14 – Apr 21, 2026"
	WorkflowChange string
	TaskTypes      []DigestTaskTypeRow
	Frictions      []DigestFrictionRow
	RawBody        string // populated when the envelope failed to parse
}

// DigestTaskTypeRow / DigestFrictionRow / DigestEvidenceRow mirror
// the prompts.Reflection* shapes with display-ready helpers
// (ShortID for evidence). Kept separate from prompts types so the
// template stays free of method calls.
type DigestTaskTypeRow struct {
	Label     string
	Frequency int
	Evidence  []DigestEvidenceRow
}

type DigestFrictionRow struct {
	Label     string
	Frequency int
	Severity  string
	Evidence  []DigestEvidenceRow
}

type DigestEvidenceRow struct {
	SessionID    string // full UUID — drives the /sessions/<id> link
	ShortID      string // 8-char preview for display
	Quote        string
	WhatHappened string
}

// SearchPage is the data shape the /search full-page template
// consumes. Most of the heavy lifting is done by the htmx
// fragment endpoint at /search/hits, so this struct is mostly
// for the layout's <title> plus seeding the filter chips that
// flow through to /search/hits via the form's hidden inputs.
type SearchPage struct {
	Title string
	// Active filter values seeded into the form so the htmx
	// fragment receives them on every keystroke. Empty when no
	// filter is set.
	ActiveAgent        string
	ActiveTool         string
	ActiveSkill        string
	ActiveFile         string
	ActiveWithFailures bool
	// FilterChips renders the active filters above the input as
	// removable chips, same UI as the sessions list. HrefRemove
	// links back to /search with that one filter dropped.
	FilterChips []FilterChip
}

// SearchHits is the shape the /search/hits fragment template
// consumes. Either Error is non-empty (parse failure, query is
// empty) and the template renders it as a muted line, or Hits
// holds the matching rows. Both empty == empty-state line.
//
// Compact is set when the fragment is rendered for the nav-bar
// popover: row count is capped tighter and the template appends a
// "see all in /search" link that re-runs the query in the full
// search page. Query is echoed back so that link can carry it.
type SearchHits struct {
	Hits    []SearchHitRow
	Error   string
	Compact bool
	Query   string
}

// SearchHitRow is one matching event for the search fragment.
type SearchHitRow struct {
	SessionID    string // full UUID for the /sessions/{id} link
	ShortID      string
	When         string // relative time
	Kind         string
	Snippet      string // SQL snippet() output, falls back to truncated content
	SummaryTopic string // parsed `topic` field from the session's summary; empty if none
}
