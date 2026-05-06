package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleSkillsStaleness serves GET /v1/skills/staleness with
// optional since_ms / window_ms / max_skills / max_examples
// filters. Backed by store.LoadSkillStaleness.
func (s *Server) handleSkillsStaleness(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return
	}

	lim := store.SkillStalenessLimits{}
	if lim.MaxSkills, ok = parsePositiveIntQuery(w, r, "max_skills", 0); !ok {
		return
	}
	if lim.MaxExamples, ok = parsePositiveIntQuery(w, r, "max_examples", 0); !ok {
		return
	}

	rows, err := store.LoadSkillStaleness(r.Context(), s.store.DB(), sinceMs, windowMs, lim)
	if err != nil {
		s.slog.Error("LoadSkillStaleness", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := wire.SkillStalenessResponse{Skills: make([]wire.SkillStaleness, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, wire.SkillStaleness{
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
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return
	}

	lim := store.SkillImpactLimits{}
	if lim.MaxSkills, ok = parsePositiveIntQuery(w, r, "max_skills", 0); !ok {
		return
	}

	rows, err := store.LoadSkillImpact(r.Context(), s.store.DB(), sinceMs, windowMs, lim)
	if err != nil {
		s.slog.Error("LoadSkillImpact", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := wire.SkillImpactResponse{Skills: make([]wire.SkillImpact, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, wire.SkillImpact{
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

	out := wire.InvokedSkillsResponse{Skills: make([]wire.InvokedSkill, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, wire.InvokedSkill{
			Name:  r.Name,
			Count: r.Count,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
