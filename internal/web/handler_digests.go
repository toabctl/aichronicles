package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
)

// digestsDefaultLimit caps how many recent digests we render. The
// cron fires weekly so even a year of history is ~52 cards; 26
// (half a year) is the sweet spot for fitting on one scroll.
const digestsDefaultLimit = 26

// digestsHandler renders /digests: cards of recent reflect_weekly
// rows from llm_outputs, newest first. Each card carries the
// covered week, the workflow_change, and collapsible sections for
// task_types and frictions. Sessions cited as evidence link back
// to /sessions/<id> so the user can verify any claim by walking
// the underlying turns.
func (s *Server) digestsHandler(w http.ResponseWriter, r *http.Request) {
	limit := digestsDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	rows, err := store.LoadLLMOutputs(r.Context(), s.store.DB(), store.LLMOutputFilter{
		Kind:  store.LLMKindReflectWeekly,
		Limit: limit,
	})
	if err != nil {
		s.log.Error("digestsHandler: load", "err", err)
		http.Error(w, "could not load digests", http.StatusInternalServerError)
		return
	}

	page := DigestsPage{
		Title:   "Digests",
		Limit:   limit,
		Digests: buildDigestCards(rows, time.Now()),
	}
	s.render(w, r, "digests", page)
}

// buildDigestCards lifts each llm_outputs row of kind=reflect_weekly
// into a render-ready DigestCard. Body parses are best-effort: a
// row with malformed JSON renders the raw body in a "raw" pane so
// the page never collapses on one broken artifact.
func buildDigestCards(rows []store.LLMOutput, now time.Time) []DigestCard {
	out := make([]DigestCard, 0, len(rows))
	for _, row := range rows {
		card := DigestCard{
			ID:          row.ID,
			Model:       row.Model,
			Generated:   relativeTime(row.CreatedAtMs, now),
			GeneratedAt: time.UnixMilli(row.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
		}

		var env prompts.WeeklyDigestEnvelope
		if err := json.Unmarshal([]byte(row.Body), &env); err == nil && env.Result != nil {
			card.Period = formatDigestPeriod(env.PeriodStart, env.PeriodEnd)
			card.WorkflowChange = env.Result.WorkflowChange
			card.TaskTypes = buildDigestTaskTypes(env.Result.TaskTypes)
			card.Frictions = buildDigestFrictions(env.Result.Frictions)
		} else {
			// Malformed envelope (older shape, hand-edited row, …):
			// fall back to dumping the body as raw text so the user
			// still sees something rather than a blank card.
			card.RawBody = row.Body
		}
		out = append(out, card)
	}
	return out
}

// formatDigestPeriod turns the RFC3339 period bounds in the
// envelope into a "Apr 14 – Apr 21, 2026" range. Falls back to
// the raw values when parsing fails — the card still renders.
func formatDigestPeriod(startISO, endISO string) string {
	start, sErr := time.Parse(time.RFC3339, startISO)
	end, eErr := time.Parse(time.RFC3339, endISO)
	if sErr != nil || eErr != nil {
		return startISO + " – " + endISO
	}
	if start.Year() == end.Year() {
		return start.Format("Jan 2") + " – " + end.Format("Jan 2, 2006")
	}
	return start.Format("2006-01-02") + " – " + end.Format("2006-01-02")
}

func buildDigestTaskTypes(in []prompts.ReflectionTaskType) []DigestTaskTypeRow {
	out := make([]DigestTaskTypeRow, 0, len(in))
	for _, t := range in {
		out = append(out, DigestTaskTypeRow{
			Label:     t.Label,
			Frequency: t.Frequency,
			Evidence:  buildDigestEvidence(t.Evidence),
		})
	}
	return out
}

func buildDigestFrictions(in []prompts.ReflectionFriction) []DigestFrictionRow {
	out := make([]DigestFrictionRow, 0, len(in))
	for _, f := range in {
		out = append(out, DigestFrictionRow{
			Label:     f.Label,
			Frequency: f.Frequency,
			Severity:  f.Severity,
			Evidence:  buildDigestEvidence(f.Evidence),
		})
	}
	return out
}

func buildDigestEvidence(in []prompts.ReflectionEvidence) []DigestEvidenceRow {
	out := make([]DigestEvidenceRow, 0, len(in))
	for _, e := range in {
		out = append(out, DigestEvidenceRow{
			SessionID:    e.SessionID,
			ShortID:      shortID(e.SessionID),
			Quote:        e.Quote,
			WhatHappened: e.WhatHappened,
		})
	}
	return out
}
