package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
)

// defaultSessionListWindow is the cutoff applied when a client
// does not supply since_ms. Generous enough that "open the web
// UI without arguments" lands on a useful list; cheap enough
// that the query is bounded.
const defaultSessionListWindow = 30 * 24 * time.Hour

// handleSessionsList serves GET /v1/sessions.
//
// Query params:
//
//   - since_ms: epoch ms cutoff against effective ts (ended_at,
//     else started_at). Sessions older are excluded. 0 / unset
//     applies a 30-day default.
//   - limit:    page size, capped at MaxPageLimit.
func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var sinceMs int64
	if v := q.Get("since_ms"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid since_ms", err.Error())
			return
		}
		if n < 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid since_ms", "must be non-negative")
			return
		}
		sinceMs = n
	} else {
		sinceMs = time.Now().Add(-defaultSessionListWindow).UnixMilli()
	}

	limit := api.DefaultPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", err.Error())
			return
		}
		if n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", "must be positive")
			return
		}
		if n > api.MaxPageLimit {
			n = api.MaxPageLimit
		}
		limit = n
	}

	rows, err := store.LoadRecentSessionDigests(r.Context(), s.store.DB(), sinceMs, limit)
	if err != nil {
		s.slog.Error("LoadRecentSessionDigests", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := api.SessionListResponse{
		Sessions: make([]api.SessionDigest, 0, len(rows)),
	}
	for _, row := range rows {
		out.Sessions = append(out.Sessions, sessionDigestRowToWire(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionsGet serves GET /v1/sessions/{id}.
func (s *Server) handleSessionsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}
	row, err := store.LoadSessionDigest(r.Context(), s.store.DB(), id)
	if err != nil {
		s.slog.Error("LoadSessionDigest", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	if row == nil {
		writeProblem(w, http.StatusNotFound, "Session not found", id)
		return
	}
	writeJSON(w, http.StatusOK, sessionDigestRowToWire(*row))
}

// handleSessionsResolve serves GET /v1/sessions/resolve?prefix=...
//
// Resolves an 8-or-more-character hex prefix to a full session_id.
// Returns 404 when no session matches and 409 when the prefix is
// ambiguous (multiple matches). MCP tools and CLIs that accept
// short prefixes use this to convert them to canonical ids.
func (s *Server) handleSessionsResolve(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		writeProblem(w, http.StatusBadRequest, "Missing prefix",
			"prefix is required")
		return
	}
	id, err := store.ResolveSessionIDPrefix(r.Context(), s.store.DB(), prefix)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoSuchSession):
			writeProblem(w, http.StatusNotFound, "Session not found", prefix)
		case errors.Is(err, store.ErrAmbiguousSessionPrefix):
			writeProblem(w, http.StatusConflict, "Ambiguous prefix", err.Error())
		default:
			// Validation errors (non-hex chars) come back as
			// plain errors with no sentinel — treat as 400 since
			// the caller's input is to blame.
			writeProblem(w, http.StatusBadRequest, "Invalid prefix", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, api.ResolveSessionResponse{ID: id})
}

// handleSessionsRelated serves GET /v1/sessions/{id}/related.
//
// Returns same-cwd prior sessions ordered newest-first, capped
// at limit (default 10, MaxPageLimit ceiling). Empty list when
// the session has no cwd, no started_at, or no prior peers.
func (s *Server) handleSessionsRelated(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "Missing session id", "")
		return
	}

	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", err.Error())
			return
		}
		if n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", "must be positive")
			return
		}
		if n > api.MaxPageLimit {
			n = api.MaxPageLimit
		}
		limit = n
	}

	rows, err := store.LoadCandidatePriorSessions(r.Context(), s.store.DB(), id, limit)
	if err != nil {
		s.slog.Error("LoadCandidatePriorSessions", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := api.CandidateSessionListResponse{
		Candidates: make([]api.CandidateSession, 0, len(rows)),
	}
	for _, c := range rows {
		out.Candidates = append(out.Candidates, api.CandidateSession{
			ID:          c.ID,
			Cwd:         c.Cwd,
			StartedAtMs: c.StartedAtMs,
			EndedAtMs:   c.EndedAtMs,
			Topic:       c.Topic,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func sessionDigestRowToWire(row store.SessionDigestRow) api.SessionDigest {
	return api.SessionDigest{
		ID:            row.ID,
		StartedAtMs:   sqlNullInt64ToPtr(row.StartedAtMs),
		EndedAtMs:     sqlNullInt64ToPtr(row.EndedAtMs),
		Cwd:           sqlNullToPtr(row.Cwd),
		FirstPrompt:   sqlNullToPtr(row.FirstPrompt),
		LatestSummary: sqlNullToPtr(row.LatestSummary),
	}
}

// sqlNullInt64ToPtr translates sql.NullInt64 to *int64 for wire
// types. Mirrors sqlNullToPtr for the int64 case.
func sqlNullInt64ToPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
