package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/wire"
)

// skillsDefaultDays is the window the /skills page covers by
// default. Same default `aichronicles skills stale` uses on the
// CLI; project-local skill discovery is bound to it (we only
// walk the cwd of sessions inside the window).
const skillsDefaultDays = 30

// skillsHandler renders /skills with three sections:
//
//  1. Installed: every SKILL.md the user has on disk under
//     ~/.claude/skills/ + project-local mirrors. Source label
//     tells you which layer it came from.
//  2. Invoked: skill_load extractions in the window, sorted by
//     count. The "this skill works" signal — patterns whose
//     work is plausibly served by an invoked skill are solved.
//  3. Stale candidates: skills whose loads correlate with a
//     tool_failure within ~10 minutes (the staleness detector
//     from `aichronicles skills stale`). These are the skills
//     most worth a `skill_manage edit` pass.
func (s *Server) skillsHandler(w http.ResponseWriter, r *http.Request) {
	days := skillsDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	installedResp, err := s.api.InstalledSkills(r.Context(), sinceMs)
	if err != nil {
		s.log.Error("skillsHandler: installed", "err", err)
		// Non-fatal: the page can still render the other two
		// sections without the installed list.
	}
	invokedResp, err := s.api.InvokedSkills(r.Context(), sinceMs)
	if err != nil {
		s.log.Error("skillsHandler: invoked", "err", err)
	}
	const staleWindowMs = int64(10 * 60 * 1000) // matches CLI default
	staleResp, err := s.api.SkillStaleness(r.Context(), wire.SkillStalenessRequest{
		SinceMs:  sinceMs,
		WindowMs: staleWindowMs,
	})
	if err != nil {
		s.log.Error("skillsHandler: staleness", "err", err)
	}

	page := SkillsPage{
		Title:     "Skills",
		Days:      days,
		Installed: installedResp.Skills,
		Invoked:   invokedResp.Skills,
		Stale:     buildStaleRows(staleResp.Skills),
	}
	s.render(w, r, "skills", page)
}

// buildStaleRows lifts wire.SkillStaleness into the rendering
// shape: short id list pre-truncated, rate as a percentage, and
// the example session ids ready to drop into /sessions/ links.
func buildStaleRows(rows []wire.SkillStaleness) []StaleSkillRow {
	out := make([]StaleSkillRow, 0, len(rows))
	for _, r := range rows {
		row := StaleSkillRow{
			Name:       r.Name,
			TotalLoads: r.TotalLoads,
			StaleLoads: r.StaleLoads,
			Rate:       int(r.Rate * 100),
		}
		for _, id := range r.Examples {
			row.Examples = append(row.Examples, StaleExample{
				SessionID: id,
				ShortID:   preview.ShortID(id),
			})
		}
		out = append(out, row)
	}
	return out
}
