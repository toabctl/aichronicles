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
	req, ok := parseSkillStalenessRequest(w, r)
	if !ok {
		return
	}
	lim := store.SkillStalenessLimits{MaxSkills: req.MaxSkills, MaxExamples: req.MaxExamples}

	rows, err := store.LoadSkillStaleness(r.Context(), s.store.DB(), req.SinceMs, req.WindowMs, lim)
	if err != nil {
		s.storeError(w, "LoadSkillStaleness", err)
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
	req, ok := parseSkillImpactRequest(w, r)
	if !ok {
		return
	}
	lim := store.SkillImpactLimits{MaxSkills: req.MaxSkills}

	rows, err := store.LoadSkillImpact(r.Context(), s.store.DB(), req.SinceMs, req.WindowMs, lim)
	if err != nil {
		s.storeError(w, "LoadSkillImpact", err)
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

// parseSkillStalenessRequest decodes + validates the
// GET /v1/skills/staleness query into wire.SkillStalenessRequest
// (server mirror of apiclient.Client.SkillStaleness).
func parseSkillStalenessRequest(w http.ResponseWriter, r *http.Request) (wire.SkillStalenessRequest, bool) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.SkillStalenessRequest{}, false
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return wire.SkillStalenessRequest{}, false
	}
	maxSkills, ok := parsePositiveIntQuery(w, r, "max_skills", 0)
	if !ok {
		return wire.SkillStalenessRequest{}, false
	}
	maxExamples, ok := parsePositiveIntQuery(w, r, "max_examples", 0)
	if !ok {
		return wire.SkillStalenessRequest{}, false
	}
	return wire.SkillStalenessRequest{
		SinceMs: sinceMs, WindowMs: windowMs,
		MaxSkills: maxSkills, MaxExamples: maxExamples,
	}, true
}

// parseSkillImpactRequest decodes + validates the GET /v1/skills/impact
// query into wire.SkillImpactRequest (server mirror of
// apiclient.Client.SkillImpact).
func parseSkillImpactRequest(w http.ResponseWriter, r *http.Request) (wire.SkillImpactRequest, bool) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.SkillImpactRequest{}, false
	}
	windowMs, ok := parseInt64Query(w, r, "window_ms")
	if !ok {
		return wire.SkillImpactRequest{}, false
	}
	maxSkills, ok := parsePositiveIntQuery(w, r, "max_skills", 0)
	if !ok {
		return wire.SkillImpactRequest{}, false
	}
	return wire.SkillImpactRequest{SinceMs: sinceMs, WindowMs: windowMs, MaxSkills: maxSkills}, true
}

// handleSkillsInstalled serves GET /v1/skills/installed: every
// SKILL.md the daemon discovers on disk — global ~/.claude/skills/
// plus project-local under any observed session cwd within the
// since_ms window. Backed by skills.CollectInstalled.
//
// since_ms is optional; zero means "every project cwd ever
// recorded" (use sparingly on large corpora — the daemon walks
// every project root). For day-to-day rendering, callers pass a
// 30-day window.
func (s *Server) handleSkillsInstalled(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	rows, err := skills.CollectInstalled(r.Context(), s.store.DB(), sinceMs)
	if err != nil {
		s.storeError(w, "skills.CollectInstalled", err)
		return
	}
	out := wire.InstalledSkillsResponse{Skills: make([]wire.InstalledSkill, 0, len(rows))}
	for _, r := range rows {
		out.Skills = append(out.Skills, wire.InstalledSkill{
			Name:        r.Name,
			Description: r.Description,
			Source:      r.Source,
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
		s.storeError(w, "skills.LoadInvoked", err)
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
