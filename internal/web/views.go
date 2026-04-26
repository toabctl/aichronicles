package web

import "html/template"

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
	FirstPrompt    string // truncated first user_prompt content_text
	HasSummary     bool   // an llm_outputs(kind='summary') row exists for this session
	SummaryTopic   string // parsed `topic` field from the cached summary; empty when none
	SummaryTooltip string // multi-line plain-text tooltip for the summary badge — topic + "what was done" bullets, native browser tooltip

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
	ID         string // full UUID
	ShortID    string
	Cwd        string
	StartedAt  string // absolute UTC, "2026-04-24T12:00:00Z"
	EndedAt    string // absolute UTC, or "(active)" if no ended_at_ms yet
	EventCount int
	Summary    *SessionSummary // nil if no cached summary
	Events     []EventRow
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

// SearchPage is the data shape the /search full-page template
// consumes. Most of the heavy lifting is done by the htmx
// fragment endpoint at /search/hits, so this struct is mostly
// for the layout's <title>.
type SearchPage struct {
	Title string
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
