package web

import (
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

// proposalsHandler renders /proposals — the lifecycle of every
// skill aichronicles has proposed via propose / induction. Same
// four-bucket categorisation the propose system prompt uses (rule
// 12) so the human's view matches the LLM's view.
//
// Buckets:
//
//   - APPLIED, in use, working — applied + post-apply loads ≥1, no
//     failures
//   - APPLIED, unused           — applied + zero post-apply loads
//   - APPLIED, failing          — applied + post-apply loads with
//     ≥1 tool_failure correlation
//   - NOT APPLIED               — never landed on disk (the user
//     declined the proposal)
//
// The page is read-only — the action surface (`propose apply`) is
// CLI-only. This view is "what has the system proposed and how did
// it go," not "do something with these."
func (s *Server) proposalsHandler(w http.ResponseWriter, r *http.Request) {
	page := ProposalsPage{Title: "Proposals"}
	now := time.Now().UTC()

	// Look back 90 days (same horizon the propose prompt's
	// "Prior proposals" stanza uses) — long enough to catch the
	// project's history without unbounded scan.
	priorSinceMs := now.Add(-90 * 24 * time.Hour).UnixMilli()

	applied, err := store.LoadProposalEffectiveness(r.Context(), s.store.DB(),
		priorSinceMs, 0, 100)
	if err != nil {
		s.log.Error("proposalsHandler: applied", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, e := range applied {
		row := ProposalRow{
			SkillName:        e.SkillName,
			ProposedAgo:      timefmt.Relative(e.ProposedAtMs, now),
			AppliedAgo:       timefmt.Relative(e.AppliedAtMs, now),
			LoadsAfterApply:  e.LoadsAfterApply,
			FailedLoadsAfter: e.FailedLoadsAfter,
			AppliedPath:      e.AppliedPath,
		}
		switch {
		case e.FailedLoadsAfter > 0:
			page.AppliedFailing = append(page.AppliedFailing, row)
		case e.LoadsAfterApply == 0:
			page.AppliedUnused = append(page.AppliedUnused, row)
		default:
			page.AppliedWorking = append(page.AppliedWorking, row)
		}
	}

	unapplied, err := store.LoadUnappliedProposedSkills(r.Context(), s.store.DB(),
		priorSinceMs, 100)
	if err != nil {
		s.log.Error("proposalsHandler: unapplied", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, u := range unapplied {
		page.NotApplied = append(page.NotApplied, ProposalRow{
			SkillName:   u.SkillName,
			ProposedAgo: timefmt.Relative(u.ProposedAtMs, now),
		})
	}
	page.UnappliedCount = len(page.NotApplied)
	s.render(w, r, "proposals", page)
}
