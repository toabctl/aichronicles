package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
)

const defaultFactsLimit = 50

// handleFactsSubjects serves GET /v1/facts/subjects?contains=...
// Returns subject strings matching the substring (case-insensitive)
// for autocomplete and exploration. The contains param is required
// — store.FactSubjectsLike rejects an empty needle to keep
// autocomplete queries scoped.
func (s *Server) handleFactsSubjects(w http.ResponseWriter, r *http.Request) {
	contains := r.URL.Query().Get("contains")
	if contains == "" {
		writeProblem(w, http.StatusBadRequest, "Missing contains",
			"contains is required so the subject lookup is scoped")
		return
	}
	limit, ok := parseLimitQuery(w, r, defaultFactsLimit)
	if !ok {
		return
	}
	subjects, err := store.FactSubjectsLike(r.Context(), s.store.DB(), contains, limit)
	if err != nil {
		s.slog.Error("FactSubjectsLike", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	if subjects == nil {
		subjects = []string{}
	}
	writeJSON(w, http.StatusOK, api.FactSubjectsResponse{Subjects: subjects})
}

// handleFactsList serves GET /v1/facts?subject=... or
// GET /v1/facts (latter returns recent facts across all subjects).
func (s *Server) handleFactsList(w http.ResponseWriter, r *http.Request) {
	subject := r.URL.Query().Get("subject")
	limit, ok := parseLimitQuery(w, r, defaultFactsLimit)
	if !ok {
		return
	}
	var rows []store.SemanticFact
	var err error
	if subject != "" {
		rows, err = store.LoadFactsForSubject(r.Context(), s.store.DB(), subject, limit)
	} else {
		rows, err = store.LoadRecentFacts(r.Context(), s.store.DB(), limit)
	}
	if err != nil {
		s.slog.Error("LoadFacts", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := api.FactsResponse{Facts: make([]api.SemanticFact, 0, len(rows))}
	for _, f := range rows {
		out.Facts = append(out.Facts, api.SemanticFact{
			ID:                f.ID,
			SourceLLMOutputID: f.SourceLLMOutputID,
			Subject:           f.Subject,
			Predicate:         f.Predicate,
			Object:            f.Object,
			Confidence:        f.Confidence,
			EvidenceSessionID: nullable.StringPtr(f.EvidenceSessionID),
			EvidenceQuote:     nullable.StringPtr(f.EvidenceQuote),
			AssertedAtMs:      f.AssertedAtMs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
