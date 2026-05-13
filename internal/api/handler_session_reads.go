package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleSessionEvents serves GET /v1/sessions/{id}/events. Returns
// every event row for the session in chronological order.
//
// Query params:
//   - limit:     positive integer, defaults to store.DefaultEventsPerSessionLimit
//   - unbounded: "true" returns every event with no LIMIT (used by
//     the segmenter, where a missing tail event would silently fix
//     the final episode's ended_at_ms at the wrong wall clock)
//
// The full content_text is shipped on the wire — callers building
// LLM prompts need it. Snippets are not generated here.
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	q := r.URL.Query()

	var limit int
	if q.Get("unbounded") == "true" {
		limit = store.LoadEventsForSessionUnbounded
	} else {
		limit = positiveOrZero(q.Get("limit"))
		if limit < 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
			return
		}
	}

	rows, err := store.LoadEventsForSession(r.Context(), s.store.DB(), id, limit)
	if err != nil {
		s.slog.Error("LoadEventsForSession", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SessionEventsResponse{Events: make([]wire.SessionEvent, 0, len(rows))}
	for _, e := range rows {
		out.Events = append(out.Events, eventViewToWire(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func eventViewToWire(e events.EventView) wire.SessionEvent {
	return wire.SessionEvent{
		EventID:      e.EventID,
		Kind:         e.Kind,
		TsSourceMs:   e.TsSourceMs,
		Role:         e.Role.Ptr(),
		ContentText:  e.ContentText.Ptr(),
		ToolName:     e.ToolName.Ptr(),
		SubagentID:   e.SubagentID.Ptr(),
		SubagentType: e.SubagentType.Ptr(),
		Cwd:          e.Cwd.Ptr(),
	}
}

// handleSessionExtractions serves GET /v1/sessions/{id}/extractions?kind=.
// kind is required ("url", "file_path", "shell_command", ...) — the
// store helper rejects empty.
func (s *Server) handleSessionExtractions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		writeProblem(w, http.StatusBadRequest, "Missing kind", "kind query param is required")
		return
	}
	rows, err := store.LoadExtractionsForSession(r.Context(), s.store.DB(), id, kind)
	if err != nil {
		s.slog.Error("LoadExtractionsForSession", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SessionExtractionsResponse{Extractions: make([]wire.Extraction, 0, len(rows))}
	for _, x := range rows {
		out.Extractions = append(out.Extractions, wire.Extraction{Kind: x.Kind, Value: x.Value})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionCandidatePriors serves
// GET /v1/sessions/{id}/candidate-priors?limit=. Returns same-cwd
// prior sessions the LLM can emit session_links for; bounded so a
// rogue candidate id from the model is silently dropped at filter
// time.
func (s *Server) handleSessionCandidatePriors(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	limit, ok := parsePositiveIntQuery(w, r, "limit", 10)
	if !ok {
		return
	}
	rows, err := store.LoadCandidatePriorSessions(r.Context(), s.store.DB(), id, limit)
	if err != nil {
		s.slog.Error("LoadCandidatePriorSessions", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.CandidateSessionListResponse{Candidates: make([]wire.CandidateSession, 0, len(rows))}
	for _, c := range rows {
		out.Candidates = append(out.Candidates, wire.CandidateSession{
			ID: c.ID, Cwd: c.Cwd, StartedAtMs: c.StartedAtMs, EndedAtMs: c.EndedAtMs, Topic: c.Topic,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionOutcome serves GET /v1/sessions/{id}/outcome. Read-
// or-backfill: the first read computes + persists; subsequent reads
// hit the cached row.
func (s *Server) handleSessionOutcome(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	row, err := store.EnsureSessionOutcome(r.Context(), s.store.DB(), id)
	if err != nil {
		s.slog.Error("EnsureSessionOutcome", "session_id", id, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	writeJSON(w, http.StatusOK, wire.SessionOutcome{
		SessionID:         row.SessionID,
		ComputedAtMs:      row.ComputedAtMs,
		UserPromptCount:   row.UserPromptCount,
		ToolUseCount:      row.ToolUseCount,
		ToolFailureCount:  row.ToolFailureCount,
		ErrorCount:        row.ErrorCount,
		CompactCount:      row.CompactCount,
		GitUndoCount:      row.GitUndoCount,
		PromptRepeatCount: row.PromptRepeatCount,
		Outcome:           string(row.Outcome),
		LastEventKind:     row.LastEventKind,
	})
}

// handleSessionDigests serves GET /v1/sessions/digests?since_ms=&limit=.
// Returns the LoadRecentSessionDigests result — every session with
// its summary topic + first prompt + cwd, used by reflect/propose
// to build a window of cross-session input.
func (s *Server) handleSessionDigests(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	limit, ok := parseLimitQuery(w, r, wire.DefaultPageLimit)
	if !ok {
		return
	}
	rows, err := store.LoadRecentSessionDigests(r.Context(), s.store.DB(), sinceMs, limit)
	if err != nil {
		s.slog.Error("LoadRecentSessionDigests", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := wire.SessionDigestsResponse{Digests: make([]wire.SessionDigest, 0, len(rows))}
	for _, row := range rows {
		out.Digests = append(out.Digests, sessionDigestRowToWire(row))
	}
	writeJSON(w, http.StatusOK, out)
}
