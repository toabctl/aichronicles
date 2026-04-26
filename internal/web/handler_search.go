package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/store"
)

// searchPageLimit caps how many hits the fragment returns. The
// htmx pattern is "type, see results, refine" — pagination would
// fight that flow. 50 is enough to confirm a query is on the
// right track without rendering huge tables.
const searchPageLimit = 50

// searchHandler renders the /search page itself: a form with the
// htmx-driven input and an empty hits container. The hits
// fragment populates the container as the user types.
func (s *Server) searchHandler(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "search", SearchPage{Title: "Search"})
}

// searchHitsHandler is the htmx fragment endpoint at
// /search/hits. Reads ?q= (and optional ?kind / ?since),
// translates q into FTS5 via internal/searchquery, and renders
// the hits fragment template — no layout, just the table or
// empty-state line that gets swapped into #hits on the page.
func (s *Server) searchHitsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	kind := r.URL.Query().Get("kind")
	since := r.URL.Query().Get("since")

	view := buildSearchHits(r, s.store, q, kind, since, s.now())
	s.renderFragment(w, "hits", view)
}

// buildSearchHits performs the search and shapes the result for
// the hits fragment. Extracted from the handler so tests can
// drive it directly without going through the HTTP layer.
func buildSearchHits(r *http.Request, st *store.Store, q, kind, since string, now time.Time) SearchHits {
	if q == "" {
		// Empty query is the page's initial state — render an
		// empty Hits with no Error so the template falls through
		// to the empty-state line.
		return SearchHits{}
	}

	fts, err := searchquery.ToFTS5(q)
	if err != nil {
		switch {
		case errors.Is(err, searchquery.ErrEmpty):
			return SearchHits{}
		case errors.Is(err, searchquery.ErrSyntax):
			return SearchHits{Error: "query: " + err.Error()}
		default:
			return SearchHits{Error: "could not parse query"}
		}
	}

	opts := store.SearchEventOpts{
		Query: fts,
		Kind:  kind,
		Limit: searchPageLimit,
		// Web defaults to relevance-with-recency-boost, same as
		// the CLI. Agents asking via MCP get OrderRecency; humans
		// browsing the web UI get rank.
		Order: store.OrderRank,
		NowMs: now.UnixMilli(),
	}
	if d, ok := parseSinceWindow(since); ok {
		opts.SinceMs = now.Add(-d).UnixMilli()
	}

	hits, err := store.SearchEvents(r.Context(), st.DB(), opts)
	if err != nil {
		return SearchHits{Error: "search failed: " + err.Error()}
	}

	out := SearchHits{Hits: make([]SearchHitRow, 0, len(hits))}
	for _, h := range hits {
		row := SearchHitRow{
			SessionID: h.SessionID,
			ShortID:   shortID(h.SessionID),
			When:      relativeTime(h.TsSourceMs, now),
			Kind:      h.Kind,
			Snippet:   pickHitSnippet(h),
		}
		out.Hits = append(out.Hits, row)
	}
	return out
}

// pickHitSnippet prefers the SQL snippet() output (centered on
// the match, tokenizer-aware) and falls back to a truncated
// content_text when snippet() returned empty — defensive: should
// not happen for hits but keeps the table from rendering blank
// cells if it ever does.
func pickHitSnippet(h store.SearchEventHit) string {
	if h.Snippet.Valid && h.Snippet.String != "" {
		return h.Snippet.String
	}
	return truncatePreview(h.Content)
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
