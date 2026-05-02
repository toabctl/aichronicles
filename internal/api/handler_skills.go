package api

import (
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
)

// handleSkillsStaleness serves GET /v1/skills/staleness with
// optional since_ms / window_ms / max_skills / max_examples
// filters. Backed by store.LoadSkillStaleness.
func (s *Server) handleSkillsStaleness(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var sinceMs, windowMs int64
	if v := q.Get("since_ms"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid since_ms", "")
			return
		}
		sinceMs = n
	}
	if v := q.Get("window_ms"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid window_ms", "")
			return
		}
		windowMs = n
	}

	lim := store.SkillStalenessLimits{}
	if v := q.Get("max_skills"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid max_skills", "")
			return
		}
		lim.MaxSkills = n
	}
	if v := q.Get("max_examples"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid max_examples", "")
			return
		}
		lim.MaxExamples = n
	}

	rows, err := store.LoadSkillStaleness(r.Context(), s.store.DB(), sinceMs, windowMs, lim)
	if err != nil {
		s.slog.Error("LoadSkillStaleness", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := api.SkillStalenessResponse{Skills: make([]api.SkillStaleness, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, api.SkillStaleness{
			Name:            r.Name,
			TotalLoads:      r.TotalLoads,
			StaleLoads:      r.StaleLoads,
			Rate:            r.Rate,
			RateLowerBound:  r.RateLowerBound,
			AutoRefineScore: r.AutoRefineScore,
			Examples:        r.Examples,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
