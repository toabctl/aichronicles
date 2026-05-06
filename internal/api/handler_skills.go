package api

import (
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
)

// handleSkillsStaleness serves GET /v1/skills/staleness with
// optional since_ms / window_ms / max_skills / max_examples
// filters. Backed by store.LoadSkillStaleness.
func (s *Server) handleSkillsStaleness(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return
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

// handleSkillsImpact serves GET /v1/skills/impact with optional
// since_ms / window_ms / max_skills filters. Sister of staleness:
// covers every loaded skill (including 100%-success ones) so
// callers can render the full distribution. Backed by
// store.LoadSkillImpact.
func (s *Server) handleSkillsImpact(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return
	}

	lim := store.SkillImpactLimits{}
	if v := q.Get("max_skills"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid max_skills", "")
			return
		}
		lim.MaxSkills = n
	}

	rows, err := store.LoadSkillImpact(r.Context(), s.store.DB(), sinceMs, windowMs, lim)
	if err != nil {
		s.slog.Error("LoadSkillImpact", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := api.SkillImpactResponse{Skills: make([]api.SkillImpact, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, api.SkillImpact{
			Name:         r.Name,
			TotalLoads:   r.TotalLoads,
			FailedLoads:  r.FailedLoads,
			SuccessRate:  r.SuccessRate,
			LastLoadedMs: r.LastLoadedMs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillsInvoked serves GET /v1/skills/invoked: per-skill
// invocation counts derived from skill_load extractions in a
// window. Backed by skills.LoadInvoked. since_ms is optional;
// zero means "all time".
func (s *Server) handleSkillsInvoked(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}

	rows, err := skills.LoadInvoked(r.Context(), s.store.DB(), sinceMs)
	if err != nil {
		s.slog.Error("skills.LoadInvoked", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := api.InvokedSkillsResponse{Skills: make([]api.InvokedSkill, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, api.InvokedSkill{
			Name:  r.Name,
			Count: r.Count,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
