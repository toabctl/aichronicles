package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

// factsIndexLimit caps how many distinct subjects /facts (no
// subject) renders. Matches the prior raw-SQL LIMIT 200; the wire
// endpoint clamps internally too.
const factsIndexLimit = 200

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
	resp, err := s.api.Facts(r.Context(), subject, 0, "")
	if err != nil {
		s.internalError(w, "factsHandler: load facts subject="+subject, "could not load facts", err)
		return
	}
	now := time.Now().UTC()
	for _, f := range resp.Facts {
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
		page.Facts = append(page.Facts, row)
	}
	s.render(w, r, "facts", page)
}
