package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleFactsSubjects serves GET /v1/facts/subjects?contains=...
//
// Two modes:
//   - contains=needle: substring match (case-insensitive) — for
//     autocomplete. Backed by store.FactSubjectsLike, capped at
//     limit (default wire.DefaultPageLimit, 50).
//   - no contains:     full distinct list — for the web's facts
//     index page. Backed by store.LoadDistinctFactSubjects, capped
//     at a larger default (200) since the consumer renders the
//     whole index, not a typeahead dropdown.
func (s *Server) handleFactsSubjects(w http.ResponseWriter, r *http.Request) {
	contains := r.URL.Query().Get("contains")
	limit, ok := parseLimitQuery(w, r, wire.DefaultPageLimit)
	if !ok {
		return
	}
	var (
		subjects []string
		err      error
	)
	if contains == "" {
		subjects, err = store.LoadDistinctFactSubjects(r.Context(), s.store.DB(), limit)
	} else {
		subjects, err = store.FactSubjectsLike(r.Context(), s.store.DB(), contains, limit)
	}
	if err != nil {
		s.storeError(w, "FactsSubjects", err)
		return
	}
	if subjects == nil {
		subjects = []string{}
	}
	writeJSON(w, http.StatusOK, wire.FactSubjectsResponse{Subjects: subjects})
}

// handleFactsList serves GET /v1/facts?subject=... or
// GET /v1/facts (latter returns recent facts across all subjects).
func (s *Server) handleFactsList(w http.ResponseWriter, r *http.Request) {
	subject := r.URL.Query().Get("subject")
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	var rows []store.SemanticFact
	var err error
	if subject != "" {
		rows, err = store.LoadFactsForSubject(r.Context(), s.store.DB(), subject, limit, offset)
	} else {
		rows, err = store.LoadRecentFacts(r.Context(), s.store.DB(), limit, offset)
	}
	if err != nil {
		s.storeError(w, "LoadFacts", err)
		return
	}
	out := wire.FactsResponse{Facts: make([]wire.SemanticFact, 0, len(rows))}
	for _, f := range rows {
		out.Facts = append(out.Facts, wire.SemanticFact{
			ID:                f.ID,
			SourceLLMOutputID: f.SourceLLMOutputID,
			Subject:           f.Subject,
			Predicate:         f.Predicate,
			Object:            f.Object,
			Confidence:        f.Confidence,
			EvidenceSessionID: f.EvidenceSessionID,
			EvidenceQuote:     f.EvidenceQuote,
			AssertedAtMs:      f.AssertedAtMs,
		})
	}
	out.NextCursor = nextCursor(offset, limit, len(rows))
	writeJSON(w, http.StatusOK, out)
}
