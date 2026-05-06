package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
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

	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	if q.Get("since_ms") == "" {
		sinceMs = time.Now().Add(-defaultSessionListWindow).UnixMilli()
	}

	limit, ok := parseLimitQuery(w, r, api.DefaultPageLimit)
	if !ok {
		return
	}

	cwd := q.Get("cwd")

	// When cwd or event_count are required, fall back to an
	// inline SQL that matches the legacy MCP list_sessions
	// shape (event_count + optional cwd filter). The plain
	// LoadRecentSessionDigests path stays the default for
	// callers that don't need either.
	if cwd != "" {
		rows, err := loadSessionsForListEndpoint(r.Context(), s.store.DB(), cwd, sinceMs, limit)
		if err != nil {
			s.slog.Error("loadSessionsForListEndpoint", "err", err)
			writeProblem(w, http.StatusInternalServerError, "Storage error", "")
			return
		}
		writeJSON(w, http.StatusOK, api.SessionListResponse{Sessions: rows})
		return
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

// loadSessionsForListEndpoint runs the cwd + since_ms + limit
// session query that backs MCP list_sessions. Inline so the api
// doesn't need to widen the legacy LoadRecentSessionDigests
// signature; consumers that need a richer shape will land in a
// dedicated store helper later.
func loadSessionsForListEndpoint(ctx context.Context, db *sql.DB, cwd string, sinceMs int64, limit int) ([]api.SessionDigest, error) {
	q := `SELECT s.id, s.started_at_ms, s.ended_at_ms, s.event_count,
		s.cwd, s.first_prompt_text, s.summary_topic
		FROM sessions s
		WHERE s.cwd = ? AND ` + store.EffectiveTsExpr + ` >= ?
		ORDER BY ` + store.EffectiveTsExpr + ` DESC
		LIMIT ?`
	rows, err := db.QueryContext(ctx, q, cwd, sinceMs, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]api.SessionDigest, 0)
	for rows.Next() {
		var id string
		var started, ended sql.NullInt64
		var ec int
		var cwdN, fp, topic sql.NullString
		if err := rows.Scan(&id, &started, &ended, &ec, &cwdN, &fp, &topic); err != nil {
			return nil, err
		}
		out = append(out, api.SessionDigest{
			ID:            id,
			StartedAtMs:   sqlNullInt64ToPtr(started),
			EndedAtMs:     sqlNullInt64ToPtr(ended),
			Cwd:           sqlNullToPtr(cwdN),
			FirstPrompt:   sqlNullToPtr(fp),
			LatestSummary: sqlNullToPtr(topic),
			EventCount:    ec,
		})
	}
	return out, rows.Err()
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

	limit, ok := parseLimitQuery(w, r, 10)
	if !ok {
		return
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
