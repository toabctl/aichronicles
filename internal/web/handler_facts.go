package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

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
		// Index mode: pull every distinct subject (capped) so the
		// user can pick one. FactSubjectsLike requires a needle, so
		// we use a SELECT DISTINCT directly to scan the corpus.
		// Empty result is a normal first-run state.
		rows, err := s.store.DB().QueryContext(r.Context(),
			`SELECT DISTINCT subject FROM semantic_facts ORDER BY subject ASC LIMIT 200`)
		if err != nil {
			s.log.Error("factsHandler: list subjects", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var sub string
			if err := rows.Scan(&sub); err != nil {
				s.log.Error("factsHandler: scan subject", "err", err)
				continue
			}
			page.Subjects = append(page.Subjects, sub)
		}
		s.render(w, r, "facts", page)
		return
	}

	// Detail mode.
	page.Subject = subject
	facts, err := store.LoadFactsForSubject(r.Context(), s.store.DB(), subject, 0)
	if err != nil {
		s.log.Error("factsHandler: load facts", "subject", subject, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
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
			row.SessionShort = shortID(*f.EvidenceSessionID)
		}
		page.Facts = append(page.Facts, row)
	}
	s.render(w, r, "facts", page)
}
