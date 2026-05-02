package api

import (
	"net/http"
	"strconv"

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
	limit := parseLimit(r, defaultFactsLimit)
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
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
	limit := parseLimit(r, defaultFactsLimit)
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
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
			EvidenceSessionID: sqlNullToPtr(f.EvidenceSessionID),
			EvidenceQuote:     sqlNullToPtr(f.EvidenceQuote),
			AssertedAtMs:      f.AssertedAtMs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// parseLimit reads the "limit" query param. Returns the default
// when missing, the parsed value when valid, or -1 to signal "the
// caller should respond with 400". Caps at MaxPageLimit.
func parseLimit(r *http.Request, def int) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return -1
	}
	if n > api.MaxPageLimit {
		return api.MaxPageLimit
	}
	return n
}
