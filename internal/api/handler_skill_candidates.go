package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleSkillCandidatesRecord serves POST /v1/skill-candidates.
// Records a new candidate or upserts metadata on an existing
// (llm_output_id, skill_name) row — same idempotency contract as
// store.RecordSkillCandidateWithMetadata.
func (s *Server) handleSkillCandidatesRecord(w http.ResponseWriter, r *http.Request) {
	var req wire.RecordSkillCandidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid body", err.Error())
		return
	}
	if req.LLMOutputID <= 0 {
		writeProblem(w, http.StatusBadRequest, "llm_output_id is required", "")
		return
	}
	if req.SkillName == "" {
		writeProblem(w, http.StatusBadRequest, "skill_name is required", "")
		return
	}
	if req.ProposedAtMs <= 0 {
		writeProblem(w, http.StatusBadRequest, "proposed_at_ms is required", "")
		return
	}

	examples := make([]store.SkillExample, 0, len(req.Metadata.Examples))
	for _, ex := range req.Metadata.Examples {
		examples = append(examples, store.SkillExample{Input: ex.Input, Output: ex.Output})
	}
	meta := store.SkillCandidateMetadata{
		Triggers: req.Metadata.Triggers,
		Tags:     req.Metadata.Tags,
		Examples: examples,
		Version:  req.Metadata.Version,
		Kind:     store.SkillKind(req.Metadata.Kind),
	}
	if err := store.RecordSkillCandidateWithMetadata(r.Context(), s.store.DB(),
		req.LLMOutputID, req.SkillName, req.ProposedAtMs, meta); err != nil {
		s.storeError(w, "RecordSkillCandidate", err)
		return
	}
	writeJSON(w, http.StatusOK, wire.RecordSkillCandidateResponse{Inserted: true})
}

// handleSkillCandidatesDecision serves POST
// /v1/skill-candidates/decision. Routes to the underlying
// MarkSkillCandidate{Added,Merged,Discarded} helper based on
// req.Decision. Maps store.ErrSkillCandidateNotFound to 404 so
// the CLI can offer a "did you forget to record?" hint.
func (s *Server) handleSkillCandidatesDecision(w http.ResponseWriter, r *http.Request) {
	var req wire.SkillCandidateDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid body", err.Error())
		return
	}
	if req.LLMOutputID <= 0 {
		writeProblem(w, http.StatusBadRequest, "llm_output_id is required", "")
		return
	}
	if req.SkillName == "" {
		writeProblem(w, http.StatusBadRequest, "skill_name is required", "")
		return
	}
	if req.DecisionAtMs <= 0 {
		writeProblem(w, http.StatusBadRequest, "decision_at_ms is required", "")
		return
	}

	var err error
	switch req.Decision {
	case wire.DecisionAdd:
		if req.AddPath == "" {
			writeProblem(w, http.StatusBadRequest, "add_path is required for decision=add", "")
			return
		}
		err = store.MarkSkillCandidateAddedWithProvenance(r.Context(), s.store.DB(),
			req.LLMOutputID, req.SkillName, req.AddPath, req.DecisionAtMs, req.BodySHA256)
	case wire.DecisionMerge:
		if req.AddPath == "" {
			writeProblem(w, http.StatusBadRequest, "add_path is required for decision=merge", "")
			return
		}
		err = store.MarkSkillCandidateMerged(r.Context(), s.store.DB(),
			req.LLMOutputID, req.SkillName, req.MergedIntoID, req.AddPath, req.DecisionAtMs)
	case wire.DecisionDiscard:
		err = store.MarkSkillCandidateDiscarded(r.Context(), s.store.DB(),
			req.LLMOutputID, req.SkillName, req.DecisionAtMs)
	default:
		writeProblem(w, http.StatusBadRequest, "Invalid decision",
			"decision must be add, merge, or discard")
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrSkillCandidateNotFound) {
			writeProblem(w, http.StatusNotFound, "Skill candidate not found",
				"no row matches (llm_output_id, skill_name) — record it first")
			return
		}
		s.slog.Error("MarkSkillCandidate", "decision", req.Decision, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.SkillCandidateDecisionResponse{})
}

// handleSkillCandidatesList serves GET /v1/skill-candidates with
// the standard ?name=&limit= filters. name is the lookup key
// callers most commonly need (the CLI's "find candidate to mark")
// flow); empty name returns 400 to keep the endpoint focused.
func (s *Server) handleSkillCandidatesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		writeProblem(w, http.StatusBadRequest, "Missing name", "name query param is required")
		return
	}
	limit := positiveOrZero(q.Get("limit"))
	if limit < 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
		return
	}

	rows, err := store.LoadSkillCandidatesByName(r.Context(), s.store.DB(), name, limit)
	if err != nil {
		s.storeError(w, "LoadSkillCandidatesByName", err)
		return
	}

	out := wire.SkillCandidatesResponse{Candidates: make([]wire.SkillCandidate, 0, len(rows))}
	for _, r := range rows {
		out.Candidates = append(out.Candidates, skillCandidateRowToWire(r))
	}
	writeJSON(w, http.StatusOK, out)
}

// skillCandidateRowToWire projects a store.SkillCandidate onto its
// wire-clean wire.SkillCandidate cousin. Centralised here so every
// handler that emits a candidate row uses the same nullable
// projection.
func skillCandidateRowToWire(r store.SkillCandidate) wire.SkillCandidate {
	out := wire.SkillCandidate{
		ID:           r.ID,
		LLMOutputID:  r.LLMOutputID,
		SkillName:    r.SkillName,
		ProposedAtMs: r.ProposedAtMs,
		Decision:     string(r.Decision),
		Triggers:     r.Triggers,
		Tags:         r.Tags,
		Version:      r.Version,
		Kind:         string(r.Kind),
	}
	out.DecisionAtMs = r.DecisionAtMs
	out.AddPath = r.AddPath
	out.MergedIntoID = r.MergedIntoID
	out.AddBodySHA256 = r.AddBodySHA256
	if len(r.Examples) > 0 {
		out.Examples = make([]wire.SkillCandidateExample, 0, len(r.Examples))
		for _, ex := range r.Examples {
			out.Examples = append(out.Examples, wire.SkillCandidateExample{
				Input: ex.Input, Output: ex.Output,
			})
		}
	}
	return out
}

// _ keeps url imported until a future request adds query encoding;
// useful as a build-time signal that wire-types changes haven't
// collapsed the import set.
var _ = url.Values{}
