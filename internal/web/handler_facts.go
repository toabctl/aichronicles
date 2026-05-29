package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
)

// factsIndexLimit caps how many distinct subjects /facts (no
// subject) renders. Matches the prior raw-SQL LIMIT 200; the wire
// endpoint clamps internally too.
const factsIndexLimit = 200

// factsPageLimit is the per-page size for a subject's facts in the
// detail view; the rest load via the "Load more" control.
const factsPageLimit = 50

// factsHandler renders /facts. Two modes:
//
//   - ?subject=<cwd>  shows every (predicate, object) for that one
//     subject. The detail page.
//   - no subject      shows the index of distinct subjects with a
//     hint about how to populate them. Lets the
//     user pick which project to drill into.
//
// Both modes share one template; the template branches on whether
// page.Subject is set.
func (s *Server) factsHandler(w http.ResponseWriter, r *http.Request) {
	page := FactsPage{Title: "Facts"}
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))

	if subject == "" {
		// Index mode: the api endpoint returns every distinct
		// semantic_facts.subject (capped). Empty result is a normal
		// first-run state and renders the "no facts yet" hint.
		resp, err := s.api.FactSubjects(r.Context(), "", factsIndexLimit)
		if err != nil {
			s.internalError(w, "factsHandler: list subjects", "internal error", err)
			return
		}
		page.Subjects = resp.Subjects
		s.render(w, r, "facts", page)
		return
	}

	// Detail mode.
	page.Subject = subject
	resp, err := s.api.Facts(r.Context(), subject, factsPageLimit, "")
	if err != nil {
		s.internalError(w, "factsHandler: load facts subject="+subject, "could not load facts", err)
		return
	}
	page.Facts = buildFactRows(resp.Facts, time.Now().UTC())
	page.NextCursor = string(resp.NextCursor)
	s.render(w, r, "facts", page)
}

// handleFactsRows serves GET /facts/rows?subject=&cursor= — the htmx
// fragment backing the facts detail "Load more" control. Same
// self-replacing-row pattern as the sessions list.
func (s *Server) handleFactsRows(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	if subject == "" {
		http.Error(w, "subject required", http.StatusBadRequest)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	resp, err := s.api.Facts(r.Context(), subject, factsPageLimit, wire.Cursor(cursor))
	if err != nil {
		s.internalError(w, "handleFactsRows: load subject="+subject, "could not load facts", err)
		return
	}
	s.renderFragment(w, "facts-rows", FactRowsView{
		Subject:    subject,
		Facts:      buildFactRows(resp.Facts, time.Now().UTC()),
		NextCursor: string(resp.NextCursor),
	})
}

// buildFactRows maps wire facts to the row view shared by the full
// detail page and the load-more fragment.
func buildFactRows(facts []wire.SemanticFact, now time.Time) []FactRow {
	out := make([]FactRow, 0, len(facts))
	for _, f := range facts {
		row := FactRow{
			Predicate:   f.Predicate,
			Object:      f.Object,
			Confidence:  int(f.Confidence * 100),
			AssertedAgo: timefmt.Relative(f.AssertedAtMs, now),
		}
		if f.EvidenceQuote != nil {
			row.Quote = *f.EvidenceQuote
		}
		if f.EvidenceSessionID != nil {
			row.SessionID = *f.EvidenceSessionID
			row.SessionShort = preview.ShortID(*f.EvidenceSessionID)
		}
		out = append(out, row)
	}
	return out
}
