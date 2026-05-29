package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/wire"
)

// searchPageLimit caps how many hits the fragment returns. The
// htmx pattern is "type, see results, refine" — pagination would
// fight that flow. 50 is enough to confirm a query is on the
// right track without rendering huge tables.
const searchPageLimit = 50

// searchCompactLimit caps results in the nav-bar popover variant.
// 8 rows fit comfortably below the input without scrolling and
// keep the popover from covering the page; users who want more
// follow the "see all" link to /search.
const searchCompactLimit = 8

// searchHandler renders the /search page itself: a form with the
// htmx-driven input and an empty hits container. The hits
// fragment populates the container as the user types.
//
// A ?q= on the URL seeds the input's value so direct navigations
// (the nav-bar popover's "see all results" link, a shared or
// bookmarked search) land with the term already filled in; the
// template's `load` trigger then fetches the hits fragment on page
// load instead of waiting for a keystroke.
//
// Faceted filters arriving on the URL (?agent=, ?tool=, …) are
// surfaced as removable chips above the input AND seeded into
// hidden form fields so each htmx GET to /search/hits carries
// them through.
func (s *Server) searchHandler(w http.ResponseWriter, r *http.Request) {
	facets := readSessionListFilters(r)
	page := SearchPage{
		Title:              "Search",
		InitialQuery:       r.URL.Query().Get("q"),
		ActiveAgent:        facets.Agent,
		ActiveTool:         facets.Tool,
		ActiveSkill:        facets.Skill,
		ActiveFile:         facets.File,
		ActiveWithFailures: facets.WithFailures,
		FilterChips:        buildSessionListChips("/search", facets),
	}
	s.render(w, r, "search", page)
}

// searchHitsHandler is the htmx fragment endpoint at
// /search/hits. Reads ?q= (and optional ?kind / ?since /
// ?compact=1) plus the same faceted filters the sessions list
// supports (?agent / ?tool / ?skill / ?file / ?with-failures),
// translates q into FTS5 via internal/searchquery, and renders
// the hits fragment template — no layout, just the table or
// empty-state line that gets swapped into #hits on the page (or
// the nav-bar popover, when ?compact=1).
func (s *Server) searchHitsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	kind := r.URL.Query().Get("kind")
	since := r.URL.Query().Get("since")
	compact := r.URL.Query().Get("compact") == "1"
	facets := readSessionListFilters(r)

	view := buildSearchHits(r, s, q, kind, since, compact, facets, s.now())
	s.renderFragment(w, "hits", view)
}

// buildSearchHits performs the search and shapes the result for
// the hits fragment. Extracted from the handler so tests can
// drive it directly without going through the HTTP layer.
func buildSearchHits(r *http.Request, s *Server, q, kind, since string, compact bool, facets sessionListFilters, now time.Time) SearchHits {
	if q == "" {
		// Empty query is the page's initial state — render an
		// empty Hits with no Error so the template falls through
		// to the empty-state line.
		return SearchHits{Compact: compact, Query: q}
	}

	// Run searchquery client-side just to detect ErrEmpty / ErrSyntax
	// up front and short-circuit the empty-state / syntax-error UX
	// without a network hop. The wire request carries the raw q;
	// the api re-parses to FTS5 server-side so every consumer
	// (CLI, MCP, web) gets the same SearchEvents path.
	if _, err := searchquery.ToFTS5(q); err != nil {
		switch {
		case errors.Is(err, searchquery.ErrEmpty):
			return SearchHits{Compact: compact, Query: q}
		case errors.Is(err, searchquery.ErrSyntax):
			return SearchHits{Error: "query: " + err.Error(), Compact: compact, Query: q}
		default:
			return SearchHits{Error: "could not parse query", Compact: compact, Query: q}
		}
	}

	limit := searchPageLimit
	if compact {
		limit = searchCompactLimit
	}
	req := wire.SearchRequest{
		Q:                 q,
		Kind:              kind,
		Limit:             limit,
		SourceAgent:       facets.Agent,
		ToolName:          facets.Tool,
		SkillName:         facets.Skill,
		FilePathSubstring: facets.File,
		WithFailures:      facets.WithFailures,
	}
	// The api defaults to recency-boosted rank order (store.OrderRank
	// is the zero value of SearchOrder) — same default the CLI and
	// web both want; no Order knob on the wire.
	if since != "" {
		d, ok := parseSinceWindow(since)
		if !ok {
			// Surface unrecognised values rather than silently
			// dropping the filter. Otherwise a typo like
			// `since=24hr` returns all-time results and the user
			// thinks the filter ran.
			return SearchHits{
				Error:   "since: unrecognised window " + since + "; valid: 24h, 7d, 30d (or empty for all time)",
				Compact: compact,
				Query:   q,
			}
		}
		req.SinceMs = now.Add(-d).UnixMilli()
	}

	resp, err := s.api.Search(r.Context(), req)
	if err != nil {
		return SearchHits{Error: "search failed: " + err.Error(), Compact: compact, Query: q}
	}
	rows := make([]SearchHitRow, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		rows = append(rows, SearchHitRow{
			SessionID: h.SessionID,
			ShortID:   preview.ShortID(h.SessionID),
			When:      relativeTime(h.TsSourceMs, now),
			Kind:      h.Kind,
			Snippet:   pickHitSnippet(h),
		})
	}

	// Compact (nav-bar popover) stays a flat, dense list: no
	// grouping, no per-session resume buttons, no topic annotation.
	if compact {
		return SearchHits{Hits: rows, Compact: true, Query: q}
	}

	// Full page: group the matches by session and decorate each
	// group header with the session's cached summary topic and
	// resume one-liners. uniqueSessionIDs preserves the hits' rank
	// order, so the strongest-matching session's group renders first.
	out := SearchHits{Query: q}
	if len(rows) == 0 {
		return out
	}
	ids := uniqueSessionIDs(rows)
	out.Groups = buildSearchSessionGroups(r, s, ids, rows)
	return out
}

// buildSearchSessionGroups folds the flat hit rows into per-session
// groups in the supplied id order, attaching each session's summary
// topic and resume one-liners to the group header.
//
// Both the summary and digest lookups soft-fail: a working search
// must not vanish because an annotation (topic) or a resume one-liner
// couldn't be resolved. On error the groups simply render without
// that decoration; the session detail page surfaces any persistent
// underlying error.
func buildSearchSessionGroups(r *http.Request, s *Server, ids []string, rows []SearchHitRow) []SearchSessionGroup {
	topics := map[string]string{}
	if summaries, err := s.api.SummariesBatch(r.Context(), ids); err == nil {
		for id, sm := range summaries {
			topics[id] = parseSummaryTopic(sm.Body)
		}
	}
	digests, err := s.api.SessionDigestsByIDs(r.Context(), ids)
	if err != nil {
		digests = nil
	}

	groupByID := make(map[string]*SearchSessionGroup, len(ids))
	groups := make([]SearchSessionGroup, len(ids))
	for i, id := range ids {
		groups[i] = SearchSessionGroup{
			SessionID:    id,
			ShortID:      preview.ShortID(id),
			SummaryTopic: topics[id],
		}
		if d, ok := digests[id]; ok {
			// Resume one-liners cd into start_cwd because
			// `claude --resume` indexes transcripts by session-start
			// cwd; fall back to the latest cwd when start_cwd is nil.
			// Same fallback the sessions list + detail page use, so
			// all three render the identical command.
			resumeCwd := d.StartCwd
			if resumeCwd == nil {
				resumeCwd = d.Cwd
			}
			groups[i].ResumeCommand = buildResumeCommandPtr(d.SourceAgent, d.SourceSessionID, resumeCwd)
			groups[i].ResumeCommandDangerous = buildResumeCommandDangerousPtr(d.SourceAgent, d.SourceSessionID, resumeCwd)
		}
		groupByID[id] = &groups[i]
	}
	for _, row := range rows {
		if g := groupByID[row.SessionID]; g != nil {
			g.Hits = append(g.Hits, row)
		}
	}
	return groups
}

// uniqueSessionIDs returns the set of distinct session IDs across
// hits in stable encounter order, so the IN-clause query in
// LoadSummariesIndexedByID stays bounded even when many hits share
// a session.
func uniqueSessionIDs(hits []SearchHitRow) []string {
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if _, ok := seen[h.SessionID]; ok {
			continue
		}
		seen[h.SessionID] = struct{}{}
		out = append(out, h.SessionID)
	}
	return out
}

// pickHitSnippet prefers the SQL snippet() output (centered on
// the match, tokenizer-aware) and falls back to a truncated
// content_text when snippet() returned empty — defensive: should
// not happen for hits but keeps the table from rendering blank
// cells if it ever does.
func pickHitSnippet(h wire.SearchHit) string {
	if h.Snippet != nil && *h.Snippet != "" {
		return *h.Snippet
	}
	if h.Content == nil {
		return "-"
	}
	return truncatePreviewString(*h.Content)
}

// parseSinceWindow turns a few preset duration strings (matching
// the <select> options on the search page) into a time.Duration.
// Anything unrecognised returns (0, false) so the caller skips
// the SinceMs filter rather than misinterpreting input.
func parseSinceWindow(s string) (time.Duration, bool) {
	switch s {
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
}

// now returns the current time. Defined as a method so tests can
// override via a Server field if they ever need deterministic
// timestamps without threading time through every helper.
func (s *Server) now() time.Time { return time.Now() }
