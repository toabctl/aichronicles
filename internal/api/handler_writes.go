package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleLLMOutputSave serves POST /v1/llm-outputs. Wraps
// store.SaveLLMOutput in a transaction (the store contract takes
// a *sql.Tx so callers can compose multi-row writes; the api
// surface is one row per call so we begin/commit per request).
//
// Idempotent on (kind, prompt_hash): a duplicate call returns
// the existing id with Inserted=false. The store applies
// redact.Outbound to Body before insertion so an LLM that leaked
// a secret into a tool result can't smuggle it past the cache
// layer.
func (s *Server) handleLLMOutputSave(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req wire.SaveLLMOutputRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
		return
	}
	// Pre-validate the fields the store would otherwise reject
	// with a sentinel-less error. Splitting validation here lets
	// transient storage failures surface as 500 (logged) and
	// schema violations as 400 (echoed to the client).
	if req.Kind == "" {
		writeProblem(w, http.StatusBadRequest, "Missing kind", "")
		return
	}
	if req.PromptHash == "" {
		writeProblem(w, http.StatusBadRequest, "Missing prompt_hash", "")
		return
	}
	if req.Body == "" {
		writeProblem(w, http.StatusBadRequest, "Missing body", "")
		return
	}

	out := &store.LLMOutput{
		Kind:        store.LLMOutputKind(req.Kind),
		Model:       req.Model,
		PromptHash:  req.PromptHash,
		Body:        req.Body,
		CreatedAtMs: req.CreatedAtMs,
	}
	out.SessionID = req.SessionID
	out.InputTokens = req.InputTokens
	out.OutputTokens = req.OutputTokens

	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		s.slog.Error("BeginTx", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	id, inserted, err := store.SaveLLMOutput(r.Context(), tx, out)
	if err != nil {
		_ = tx.Rollback()
		s.slog.Error("SaveLLMOutput", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	if err := tx.Commit(); err != nil {
		s.slog.Error("Commit", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.SaveLLMOutputResponse{ID: id, Inserted: inserted})
}

// handleEpisodesSave serves POST /v1/episodes. Replaces every
// episode for the named session atomically (DELETE-then-INSERT
// inside one tx in the store).
func (s *Server) handleEpisodesSave(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req wire.SaveEpisodesRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
		return
	}
	if req.SessionID == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session_id", "")
		return
	}
	eps := make([]events.Episode, 0, len(req.Episodes))
	for _, e := range req.Episodes {
		eps = append(eps, episodeFromWire(e))
	}
	n, err := store.SaveEpisodes(r.Context(), s.store.DB(), req.SessionID, eps)
	if err != nil {
		s.slog.Error("SaveEpisodes", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.SaveEpisodesResponse{Saved: n})
}

// handleFactsSave serves POST /v1/facts.
func (s *Server) handleFactsSave(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req wire.SaveSemanticFactRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
		return
	}
	// Pre-validate so storage failures don't get mis-labelled as
	// client validation errors.
	if req.SourceLLMOutputID <= 0 {
		writeProblem(w, http.StatusBadRequest, "Missing source_llm_output_id", "must be > 0")
		return
	}
	if req.Subject == "" {
		writeProblem(w, http.StatusBadRequest, "Missing subject", "")
		return
	}
	if req.Predicate == "" {
		writeProblem(w, http.StatusBadRequest, "Missing predicate", "")
		return
	}

	f := store.SemanticFact{
		SourceLLMOutputID: req.SourceLLMOutputID,
		Subject:           req.Subject,
		Predicate:         req.Predicate,
		Object:            req.Object,
		Confidence:        req.Confidence,
		AssertedAtMs:      req.AssertedAtMs,
	}
	f.EvidenceSessionID = req.EvidenceSessionID
	f.EvidenceQuote = req.EvidenceQuote
	id, err := store.SaveSemanticFact(r.Context(), s.store.DB(), f)
	if err != nil {
		s.slog.Error("SaveSemanticFact", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.SaveSemanticFactResponse{ID: id})
}

// handleSessionOutcomeSave serves POST /v1/session-outcomes.
func (s *Server) handleSessionOutcomeSave(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req wire.SaveSessionOutcomeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
		return
	}
	o := store.SessionOutcome{
		SessionID:         req.SessionID,
		ComputedAtMs:      req.ComputedAtMs,
		UserPromptCount:   req.UserPromptCount,
		ToolUseCount:      req.ToolUseCount,
		ToolFailureCount:  req.ToolFailureCount,
		ErrorCount:        req.ErrorCount,
		CompactCount:      req.CompactCount,
		GitUndoCount:      req.GitUndoCount,
		PromptRepeatCount: req.PromptRepeatCount,
		Outcome:           store.OutcomeLabel(req.Outcome),
	}
	o.LastEventKind = req.LastEventKind
	if err := store.SaveSessionOutcome(r.Context(), s.store.DB(), o); err != nil {
		// Distinguish "missing session" (FK violation surfaces as
		// the readable "session does not exist" error from the
		// store) from generic storage errors.
		if errors.Is(err, store.ErrSessionNotFound) {
			writeProblem(w, http.StatusBadRequest, "Session does not exist", req.SessionID)
			return
		}
		s.slog.Error("SaveSessionOutcome", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionLinksSave serves POST /v1/session-links. Replaces
// every outgoing link from the named session atomically.
func (s *Server) handleSessionLinksSave(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req wire.SaveSessionLinksRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
		return
	}
	if req.FromSessionID == "" {
		writeProblem(w, http.StatusBadRequest, "Missing from_session_id", "")
		return
	}
	// Pre-validate the link constraints so transient storage
	// failures don't get mis-labelled as 400. SaveSessionLinks
	// also validates these, but the surface error is sentinel-
	// less so we can't distinguish in the err handler. Mirror
	// its checks here; any error returned from the store after
	// these pass is a real storage failure.
	for i, l := range req.Links {
		if l.ToSessionID == "" {
			writeProblem(w, http.StatusBadRequest, "Invalid link",
				fmt.Sprintf("link[%d].to_session_id is empty", i))
			return
		}
		if l.ToSessionID == req.FromSessionID {
			writeProblem(w, http.StatusBadRequest, "Invalid link",
				"self-link not allowed")
			return
		}
		if !store.IsValidSessionLinkKind(l.Kind) {
			writeProblem(w, http.StatusBadRequest, "Invalid link kind",
				"link["+strconv.Itoa(i)+"].kind="+l.Kind)
			return
		}
	}

	links := make([]store.SessionLink, 0, len(req.Links))
	for _, l := range req.Links {
		links = append(links, store.SessionLink{
			FromSessionID: req.FromSessionID,
			ToSessionID:   l.ToSessionID,
			Kind:          l.Kind,
			Rationale:     l.Rationale,
			CreatedAtMs:   l.CreatedAtMs,
		})
	}
	if err := store.SaveSessionLinks(r.Context(), s.store.DB(), req.FromSessionID, links); err != nil {
		s.slog.Error("SaveSessionLinks", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// episodeFromWire is the inverse of episodeToWire — projects the
// JSON-clean wire.Episode back to the events.Episode the store
// SaveEpisodes call expects.
func episodeFromWire(e wire.Episode) events.Episode {
	out := events.Episode{
		ID:            e.ID,
		SessionID:     e.SessionID,
		Ordinal:       e.Ordinal,
		StartedAtMs:   e.StartedAtMs,
		EndedAtMs:     e.EndedAtMs,
		IntentSummary: e.IntentSummary,
		EventCount:    e.EventCount,
		FirstEventID:  e.FirstEventID,
	}
	if e.Cwd != nil {
		out.Cwd = events.NullString{String: *e.Cwd, Valid: true}
	}
	return out
}
