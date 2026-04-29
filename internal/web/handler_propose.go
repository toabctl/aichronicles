package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// proposeDefaultLimit caps how many propose rows /propose renders.
// Propose is a manual command (no cron), so the user typically has
// a handful of these rather than the weekly digest's open-ended
// stream. 20 covers a year of weekly-cadence runs without paging.
const proposeDefaultLimit = 20

// proposeHandler renders /propose: cards of cached propose outputs,
// newest first. Each card lists the skills the model proposed,
// each skill with its evidence sessions linked back to
// /sessions/<id> and a copy-to-clipboard button carrying the exact
// `aichronicles propose apply --skill <name> --output-id <id>`
// command — explicit --output-id so copy-paste works even when a
// newer propose row has landed since the page was rendered.
//
// We deliberately do NOT apply skills from the browser. The web
// UI is read-only; touching ~/.claude/skills/ from a localhost
// service would break that contract and turn one accidental click
// into a filesystem mutation. The copy-paste flow keeps the human
// in the loop.
func (s *Server) proposeHandler(w http.ResponseWriter, r *http.Request) {
	limit := proposeDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	rows, err := store.LoadLLMOutputs(r.Context(), s.store.DB(), store.LLMOutputFilter{
		Kind:  store.LLMKindPropose,
		Limit: limit,
	})
	if err != nil {
		s.log.Error("proposeHandler: load", "err", err)
		http.Error(w, "could not load proposals", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	page := ProposePage{
		Title:     "Propose",
		Limit:     limit,
		Proposals: buildProposeCards(rows, now),
	}
	if err := loadProposalLifecycle(r.Context(), s, &page, now); err != nil {
		s.log.Error("proposeHandler: lifecycle", "err", err)
		// Best-effort: render the page with empty lifecycle
		// sections rather than fail the whole route — the user
		// still wants the recent-runs view.
	}
	s.render(w, r, "propose", page)
}

// loadProposalLifecycle fills the four lifecycle buckets on a
// ProposePage from the skill_candidates table. Same horizon (90d)
// the propose system prompt's "Prior proposals" stanza uses, so
// the human's view aligns with the LLM's prior view.
func loadProposalLifecycle(ctx context.Context, s *Server, page *ProposePage, now time.Time) error {
	priorSinceMs := now.Add(-90 * 24 * time.Hour).UnixMilli()

	added, err := store.LoadSkillCandidateEffectiveness(ctx, s.store.DB(),
		priorSinceMs, 0, 100)
	if err != nil {
		return fmt.Errorf("added: %w", err)
	}
	for _, e := range added {
		row := ProposalRow{
			SkillName:        e.SkillName,
			ProposedAgo:      relativeTime(e.ProposedAtMs, now),
			AddedAgo:         relativeTime(e.AddedAtMs, now),
			LoadsAfterAdd:    e.LoadsAfterAdd,
			FailedLoadsAfter: e.FailedLoadsAfter,
			AddPath:          e.AddPath,
		}
		switch {
		case e.FailedLoadsAfter > 0:
			page.AddedFailing = append(page.AddedFailing, row)
		case e.LoadsAfterAdd == 0:
			page.AddedUnused = append(page.AddedUnused, row)
		default:
			page.AddedWorking = append(page.AddedWorking, row)
		}
	}

	pending, err := store.LoadPendingSkillCandidates(ctx, s.store.DB(),
		priorSinceMs, 100)
	if err != nil {
		return fmt.Errorf("pending: %w", err)
	}
	for _, u := range pending {
		page.Pending = append(page.Pending, ProposalRow{
			SkillName:   u.SkillName,
			ProposedAgo: relativeTime(u.ProposedAtMs, now),
		})
	}
	page.PendingCount = len(page.Pending)
	return nil
}

// buildProposeCards lifts each kind=propose row into a render-ready
// ProposeCard. Body parses are best-effort: a row whose JSON
// doesn't parse falls through to the raw-body branch so a single
// malformed artifact doesn't blank the page.
func buildProposeCards(rows []store.LLMOutput, now time.Time) []ProposeCard {
	out := make([]ProposeCard, 0, len(rows))
	for _, row := range rows {
		card := ProposeCard{
			ID:          row.ID,
			Model:       row.Model,
			Generated:   relativeTime(row.CreatedAtMs, now),
			GeneratedAt: time.UnixMilli(row.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
		}

		var result prompts.ProposalResult
		if err := json.Unmarshal([]byte(row.Body), &result); err == nil {
			card.Skills = buildProposeSkills(result.Skills, row.ID)
		} else {
			card.RawBody = row.Body
		}
		out = append(out, card)
	}
	return out
}

// buildProposeSkills lifts a slice of prompts.ProposedSkill into
// the per-template render shape. AddCmd is the canonical
// `propose add` command for this skill — always with --output-id
// so the copy-paste survives newer propose runs.
func buildProposeSkills(skills []prompts.ProposedSkill, outputID int64) []ProposeSkillRow {
	out := make([]ProposeSkillRow, 0, len(skills))
	for _, s := range skills {
		out = append(out, ProposeSkillRow{
			Name:                 s.Name,
			WhenToUse:            s.WhenToUse,
			Why:                  s.Why,
			Frequency:            s.Frequency,
			Effort:               s.Effort,
			AlternativesRejected: s.AlternativesRejected,
			Scripts:              buildProposeScripts(s.Scripts),
			Evidence:             buildProposeEvidence(s.Evidence),
			AddCmd: fmt.Sprintf("aichronicles propose add --skill %s --output-id %d",
				s.Name, outputID),
		})
	}
	return out
}

func buildProposeScripts(in []prompts.ProposedSkillScript) []ProposeScriptRow {
	out := make([]ProposeScriptRow, 0, len(in))
	for _, sc := range in {
		out = append(out, ProposeScriptRow{
			Name:    sc.Name,
			Purpose: sc.Purpose,
		})
	}
	return out
}

func buildProposeEvidence(in []prompts.ProposalEvidence) []ProposeEvidenceRow {
	out := make([]ProposeEvidenceRow, 0, len(in))
	for _, e := range in {
		out = append(out, ProposeEvidenceRow{
			SessionID:    e.SessionID,
			ShortID:      shortID(e.SessionID),
			Quote:        e.Quote,
			WhatHappened: e.WhatHappened,
		})
	}
	return out
}
