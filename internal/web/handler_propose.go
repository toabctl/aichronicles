package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/skillscaffold"
	"github.com/toabctl/aichronicles/internal/wire"
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
// `aichronicles propose add --skill <name> --output-id <id>`
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

	rows, err := s.api.LLMOutputsList(r.Context(), string(wire.LLMKindPropose), "", limit)
	if err != nil {
		s.internalError(w, "proposeHandler: load", "could not load proposals", err)
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

// proposeDetailHandler renders /propose/{id}/{skill}: a read-only
// preview of the exact SKILL.md (plus helper scripts) that
// `aichronicles propose add --skill <name> --output-id <id>` would
// write. The skill name alone isn't a unique key — the same name
// can recur across propose runs — so the route is keyed on the
// llm_outputs id AND the name, matching the (--output-id, --skill)
// pair the CLI uses.
//
// The preview is rendered by internal/skillscaffold, the same code
// the CLI writes from, so it is byte-identical to the file the user
// would get. Crucially it runs the SAME anti-fabrication grounding
// the CLI's loadLatestProposal does (drop ungrounded triggers and
// unresolvable evidence sessions) BEFORE rendering — otherwise the
// preview would show citations the `add` path strips, and the
// preview and the written file would disagree.
//
// This stays read-only: like the rest of /propose, it never touches
// ~/.claude/skills/. It only shows what the CLI would produce.
func (s *Server) proposeDetailHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	skillName := r.PathValue("skill")

	out, err := s.api.LLMOutputByID(r.Context(), id)
	switch {
	case errors.Is(err, apiclient.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.internalError(w, fmt.Sprintf("proposeDetailHandler: load id=%d", id), "could not load proposal", err)
		return
	}
	// A non-propose row at this id is a 404, not a 500: the URL is
	// well-formed, the resource just isn't a proposal.
	if out.Kind != string(wire.LLMKindPropose) {
		http.NotFound(w, r)
		return
	}

	var result prompts.ProposalResult
	if err := json.Unmarshal([]byte(out.Body), &result); err != nil {
		s.internalError(w, fmt.Sprintf("proposeDetailHandler: parse id=%d", id), "proposal body is not valid JSON", err)
		return
	}
	// Anti-fabrication grounding, mirroring cli.loadLatestProposal:
	// drop triggers the LLM emitted without anchoring in evidence,
	// then drop evidence whose SessionID doesn't resolve to a real
	// session. Order matters — GroundEvidence after GroundTriggers.
	result.GroundTriggers()
	result.GroundEvidence(s.resolveProposalEvidence(r.Context(), &result))

	sk := findProposedSkillByName(&result, skillName)
	if sk == nil {
		http.NotFound(w, r)
		return
	}

	rendered := skillscaffold.Render(sk, out.ID)
	scripts := make([]ProposeDetailScript, 0, len(sk.Scripts))
	for i := range sk.Scripts {
		scripts = append(scripts, ProposeDetailScript{
			Name: sk.Scripts[i].Name,
			Body: skillscaffold.RenderScript(&sk.Scripts[i], sk, out.ID),
		})
	}

	now := time.Now()
	page := ProposeDetailPage{
		Title:       "Preview · " + sk.Name,
		OutputID:    out.ID,
		Model:       out.Model,
		Generated:   relativeTime(out.CreatedAtMs, now),
		GeneratedAt: time.UnixMilli(out.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
		SkillName:   sk.Name,
		WhenToUse:   sk.WhenToUse,
		Why:         sk.Why,
		AddCmd:      fmt.Sprintf("aichronicles propose add --skill %s --output-id %d", sk.Name, out.ID),
		SkillMd:     rendered.Full,
		Scripts:     scripts,
	}
	s.render(w, r, "propose_detail", page)
}

// findProposedSkillByName returns the skill in r whose name matches
// name exactly, or nil. Unlike the CLI's findProposedSkill there is
// no prefix-match fallback: the detail link always carries the
// canonical kebab-case name, so an exact match is both sufficient
// and unambiguous.
func findProposedSkillByName(r *prompts.ProposalResult, name string) *prompts.ProposedSkill {
	for i := range r.Skills {
		if r.Skills[i].Name == name {
			return &r.Skills[i]
		}
	}
	return nil
}

// resolveProposalEvidence returns the subset of distinct evidence
// SessionIDs across r.Skills that resolve to a real session via the
// api. Mirrors cli.resolveEvidenceSessions: anything that fails to
// resolve (not found, transport error) is omitted so the rendered
// SKILL.md never cites a session the user can't open. One round-trip
// per distinct id; propose payloads cite ≤25 sessions so the cost is
// bounded by the proposal size.
func (s *Server) resolveProposalEvidence(ctx context.Context, r *prompts.ProposalResult) map[string]struct{} {
	seen := map[string]struct{}{}
	for i := range r.Skills {
		for _, e := range r.Skills[i].Evidence {
			id := strings.TrimSpace(e.SessionID)
			if id == "" {
				continue
			}
			seen[id] = struct{}{}
		}
	}
	out := make(map[string]struct{}, len(seen))
	for id := range seen {
		if _, err := s.api.Session(ctx, id); err == nil {
			out[id] = struct{}{}
		}
	}
	return out
}

// loadProposalLifecycle fills the four lifecycle buckets on a
// ProposePage from the skill_candidates table. Same horizon (90d)
// the propose system prompt's "Prior proposals" stanza uses, so
// the human's view aligns with the LLM's prior view.
func loadProposalLifecycle(ctx context.Context, s *Server, page *ProposePage, now time.Time) error {
	priorSinceMs := now.Add(-90 * 24 * time.Hour).UnixMilli()

	addedResp, err := s.api.SkillCandidatesEffectiveness(ctx, wire.SkillCandidateEffectivenessRequest{
		SinceMs: priorSinceMs,
		Limit:   100,
	})
	if err != nil {
		return fmt.Errorf("added: %w", err)
	}
	for _, e := range addedResp.Candidates {
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

	pendingResp, err := s.api.SkillCandidatesPending(ctx, priorSinceMs, 100)
	if err != nil {
		return fmt.Errorf("pending: %w", err)
	}
	for _, u := range pendingResp.Candidates {
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
func buildProposeCards(rows []wire.LLMOutput, now time.Time) []ProposeCard {
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
			OutputID:             outputID,
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
			ShortID:      preview.ShortID(e.SessionID),
			Quote:        e.Quote,
			WhatHappened: e.WhatHappened,
		})
	}
	return out
}
