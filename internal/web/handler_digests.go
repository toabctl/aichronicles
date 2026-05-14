package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/wire"
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

	rows, err := s.api.LLMOutputsList(r.Context(), string(wire.LLMKindReflectWeekly), "", limit)
	if err != nil {
		s.internalError(w, "digestsHandler: load", "could not load digests", err)
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
// row whose JSON doesn't decode into a ReflectionResult renders the
// raw body in a collapsible pane so the page never collapses on
// one broken artifact.
//
// The persisted body is the bare prompts.ReflectionResult — the
// same shape `aichronicles digest weekly` writes (see
// internal/cli/digest.go: the envelope wrapper is computed at
// render time, not persisted, to avoid double-wrapping cache hits).
// The CLI's decodeStoredEnvelope reads the same shape; this is
// the matching web-side reader.
func buildDigestCards(rows []wire.LLMOutput, now time.Time) []DigestCard {
	out := make([]DigestCard, 0, len(rows))
	for _, row := range rows {
		card := DigestCard{
			ID:          row.ID,
			Model:       row.Model,
			Generated:   relativeTime(row.CreatedAtMs, now),
			GeneratedAt: time.UnixMilli(row.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
		}

		var result prompts.ReflectionResult
		parsed := json.Unmarshal([]byte(row.Body), &result) == nil
		// A `null` body or `{}` parses cleanly but leaves the
		// struct zero-valued — guard so an empty row doesn't
		// silently render as a blank card. At least one of the
		// three reflection fields must be populated for the parse
		// to count.
		populated := len(result.TaskTypes) > 0 ||
			len(result.Frictions) > 0 ||
			result.WorkflowChange != ""
		if parsed && populated {
			card.WorkflowChange = result.WorkflowChange
			card.TaskTypes = buildDigestTaskTypes(result.TaskTypes)
			card.Frictions = buildDigestFrictions(result.Frictions)
		} else {
			// Malformed or empty body (older shape, hand-edited
			// row, schema drift): fall back to dumping the raw
			// text so the user can see what's there.
			card.RawBody = row.Body
		}
		out = append(out, card)
	}
	return out
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
			ShortID:      preview.ShortID(e.SessionID),
			Quote:        e.Quote,
			WhatHappened: e.WhatHappened,
		})
	}
	return out
}
