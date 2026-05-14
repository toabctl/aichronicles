package web

import (
	"html/template"

	"github.com/toabctl/aichronicles/internal/wire"
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

	// RelatedSessions groups outgoing+incoming session_links into
	// the four canonical kinds for the sidebar. Empty groups stay
	// in the slice with no entries; the template hides them. Nil
	// when neither direction has any links.
	RelatedSessions []RelatedSessionGroup

	// Episodes is the per-session segmenter output: each entry is
	// one bounded run within the session where the user pursued
	// one intent. Nil/empty when the daemon hasn't segmented this
	// session yet (the candidate filter is identical to induction's,
	// so a session that hasn't been induced won't have episodes).
	Episodes []EpisodeRow
}

// EpisodeRow is one row of the Episodes section on the session
// detail page. Pre-rendered for the template so the template stays
// formatting-free. Mirrors the fields the user actually reads:
// ordinal as a recognisable label, time bracket as relative-since,
// cwd if any, and the intent summary as the first user prompt
// clipped to MaxEpisodeIntentSummaryRunes.
type EpisodeRow struct {
	Ordinal       int
	Started       string // relative since-now, e.g. "3h ago"
	Ended         string // relative since-now, e.g. "2h ago"
	Cwd           string // empty when episode has no cwd
	IntentSummary string // first user prompt, clipped at the store layer
	EventCount    int
}

// RelatedSessionGroup is one kind's worth of links rendered into
// the session-detail sidebar. Direction is "out" when this session
// is the from-side, "in" when it's the to-side — surfaced in the
// template so the user can tell "this builds on X" from "X builds
// on this". Display is deliberately small: short id + topic +
// rationale. The template links the short id back to the full
// /sessions/<id> page.
type RelatedSessionGroup struct {
	Kind    string // "builds_on", "repeats_failure_of", "supersedes", "related"
	Label   string // display label, e.g. "Builds on" / "Builds on this"
	Entries []RelatedSessionEntry
}

type RelatedSessionEntry struct {
	Direction string // "out" or "in"
	ID        string // full UUID
	ShortID   string
	Topic     string // empty when the linked session has no summary
	Rationale string
}

// SessionSummary is the rendered shape of an llm_outputs row of
// kind='summary'. Mirrors prompts.SummaryResult one-for-one but
// kept in this package so the template doesn't reach into
// internal/llm/prompts directly.
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
//
// LoadErrors carries one entry per section whose apiclient call
// failed. The page renders without that section's data but the
// template surfaces a banner so the user can tell "empty" from
// "broken" — previously a daemon error silently rendered as
// "no installed skills" with no indication anything went wrong.
type SkillsPage struct {
	Title      string
	Days       int
	Installed  []wire.InstalledSkill
	Invoked    []wire.InvokedSkill
	Stale      []StaleSkillRow
	LoadErrors []string
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

// FactsPage is the data shape /facts consumes. Two modes:
//   - subject==""  → render the index of distinct subjects, one
//     chip per subject the user can click
//   - subject!=""  → render every fact for that one subject
//
// Pre-computed AssertedAgo / sorted by predicate so the template
// stays free of formatting helpers.
type FactsPage struct {
	Title    string
	Subject  string    // when set, page shows facts for this subject only
	Subjects []string  // index mode: distinct subjects to choose from
	Facts    []FactRow // detail mode: facts for the selected subject
}

// FactRow is one row in the facts table for the detail page.
type FactRow struct {
	Predicate    string
	Object       string
	Confidence   int    // 0–100 percent
	AssertedAgo  string // "2d ago"
	Quote        string // evidence_quote, may be empty
	SessionID    string // evidence session, full id (for /sessions/<id>)
	SessionShort string // 8-char preview
}

// WorkflowsPage is the data shape /workflows consumes. One row per
// induction row whose body.workflow is non-null. Pre-rendered
// procedure preview keeps the template lean.
type WorkflowsPage struct {
	Title     string
	Workflows []WorkflowRow
}

// WorkflowRow is one row in the workflows list. ProcedurePreview is
// the steps joined with " → "; PreconditionsLine is them joined
// with "; " for inline rendering.
type WorkflowRow struct {
	SessionID     string
	SessionShort  string
	InducedAgo    string
	TaskShape     string
	Procedure     []string // each step's Action, in order
	Preconditions []string
	SuccessChecks []string
}

// ProposalRow is one row in a proposals lifecycle table.
// Embedded into ProposePage; the lifecycle view lives on /propose
// alongside the recent-runs view. Field names follow the AutoSkill
// (Yang et al., 2026 — arXiv:2603.01145) maintenance vocabulary —
// AddedAgo / LoadsAfterAdd / AddPath — so the rendering layer
// matches the data model and the LLM-facing prompt strings.
type ProposalRow struct {
	SkillName        string
	ProposedAgo      string
	AddedAgo         string // empty when the candidate is pending
	LoadsAfterAdd    int
	FailedLoadsAfter int
	AddPath          string
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
	Overview       wire.InsightsOverview
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
// WidthBucket is a 0–10 bucket of the busiest hour's count, so
// the template can render `class="bar bar-w-{{.WidthBucket}}"` —
// inline `style="width:…"` would be blocked by the strict CSP
// (style-src 'self' without 'unsafe-inline').
type InsightsHourRow struct {
	Hour        int
	Count       int
	WidthBucket int // 0–10, maps to a .bar-w-N CSS class
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

// DigestCard is one rendered digest. WorkflowChange / TaskTypes /
// Frictions land when the persisted body parses as a
// ReflectionResult; RawBody holds the unparsed JSON when it
// doesn't, so a single bad artifact doesn't break the whole page.
// The covered week isn't persisted on the row (the writer keeps
// the body as a bare ReflectionResult, by design — see
// buildDigestCards) so the card title falls back to "digest #ID"
// in the template; Generated / GeneratedAt give the timestamp.
type DigestCard struct {
	ID             int64
	Model          string
	Generated      string // relative ("3d ago")
	GeneratedAt    string // absolute UTC
	WorkflowChange string
	TaskTypes      []DigestTaskTypeRow
	Frictions      []DigestFrictionRow
	RawBody        string // populated when the body failed to parse
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

// ProposePage drives /propose. The page renders TWO views in
// sections:
//
//  1. Lifecycle (top section) — every skill aichronicles has
//     proposed, bucketed by what happened next. Same four-bucket
//     categorisation the propose system prompt uses (rule 12) so
//     the human's view mirrors the LLM's prior-proposals stanza.
//
//  2. Recent runs (bottom section) — cards of cached propose
//     LLM-outputs (kind=propose), newest first. Each card lists
//     the skills the model suggested with evidence + a
//     copy-to-clipboard `aichronicles propose add` command.
//
// One page rather than two routes (the previous /propose +
// /proposals split) because both views answer questions in the
// same mental model — "the propose pipeline" — and a user landing
// on one half always wanted a peek at the other.
type ProposePage struct {
	Title     string
	Limit     int
	Proposals []ProposeCard

	// Lifecycle of past skill candidates, bucketed by AutoSkill
	// (Yang et al., 2026) maintenance state. Empty slices when the
	// store has nothing in that bucket — the template hides the
	// section in that case.
	AddedWorking []ProposalRow
	AddedUnused  []ProposalRow
	AddedFailing []ProposalRow
	Pending      []ProposalRow
	PendingCount int
}

// ProposeCard is one rendered propose row. RawBody is populated
// when the persisted ProposalResult JSON didn't parse; the rest
// stays empty in that case so the template knows to fall through
// to a "show raw" pane rather than render an empty card.
type ProposeCard struct {
	ID          int64
	Model       string
	Generated   string // relative ("2d ago")
	GeneratedAt string // absolute UTC for the title attr
	Skills      []ProposeSkillRow
	RawBody     string
}

// ProposeSkillRow mirrors prompts.ProposedSkill with display-ready
// helpers (AddCmd already shaped, evidence pre-shortened, etc.).
type ProposeSkillRow struct {
	Name                 string
	WhenToUse            string
	Why                  string
	Frequency            int
	Effort               string
	AlternativesRejected string
	Scripts              []ProposeScriptRow
	Evidence             []ProposeEvidenceRow
	// AddCmd is the exact `aichronicles propose add --skill X
	// --output-id N` line the copy button drops onto the user's
	// clipboard. --output-id is always set so the copy-paste
	// survives later propose runs.
	AddCmd string
}

type ProposeScriptRow struct {
	Name    string
	Purpose string
}

type ProposeEvidenceRow struct {
	SessionID    string
	ShortID      string
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
