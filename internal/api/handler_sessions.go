package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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
	req, offset, ok := parseSessionListRequest(w, r)
	if !ok {
		return
	}

	facets := store.SessionListFacets{
		Cwd:               req.Cwd,
		SourceAgent:       req.SourceAgent,
		Project:           req.Project,
		ToolName:          req.ToolName,
		SkillName:         req.SkillName,
		FilePathSubstring: req.FilePathSubstring,
		WithFailures:      req.WithFailures,
		WithoutSummary:    req.WithoutSummary,
	}

	// When any filter is set we run the rich list query that
	// supports faceted EXISTS clauses + event_count + first_prompt.
	// One loader for every request, filtered or not.
	//
	// This used to branch to LoadRecentSessionDigests when no facet
	// was set, on the theory that it was the cheaper read. The two
	// loaders project different columns, so the endpoint silently
	// changed shape depending on whether the caller happened to pass
	// a filter: event_count came back 0 for every row (the plain
	// loader does not select it at all) and latest_summary carried
	// the whole summary JSON body instead of just the topic.
	//
	// The consequence was visible everywhere the default list is
	// rendered — `aichronicles sessions` printed 0 in its EVENTS
	// column until you added --cwd, and the web sessions page and MCP
	// list_sessions did the same. It also made
	// wire.SessionDigest.EventCount's "populated by GET /v1/sessions"
	// doc exactly backwards for that endpoint's default shape.
	//
	// The faceted loader with no facets is also the cheaper query: it
	// reads materialised columns where the plain one runs a
	// correlated subquery for latest_summary.
	//
	// Note /v1/sessions/digests is a different endpoint with a
	// different contract — it feeds reflect/digest, which need the
	// full summary body — and still uses LoadRecentSessionDigests.
	rows, err := store.LoadSessionsForListFaceted(
		r.Context(), s.store.DB(), facets, req.SinceMs, req.Limit, offset)
	if err != nil {
		s.storeError(w, "LoadSessionsForListFaceted", err)
		return
	}

	out := wire.SessionListResponse{Sessions: make([]wire.SessionDigest, 0, len(rows))}
	for _, row := range rows {
		out.Sessions = append(out.Sessions, sessionDigestRowToWire(row))
	}
	out.NextCursor = nextCursor(offset, req.Limit, len(rows))
	writeJSON(w, http.StatusOK, out)
}

// parseSessionListRequest decodes + validates the GET /v1/sessions
// query into the canonical wire.SessionListRequest (the server mirror
// of apiclient.Client.Sessions, which builds the same query from the
// same struct). Returns the request, the decoded page offset (cursor
// is opaque on the wire; offset is its decoded form), and ok=false
// after a 400 has been written. SinceMs defaults to a 30-day window
// when the client omits it.
func parseSessionListRequest(w http.ResponseWriter, r *http.Request) (wire.SessionListRequest, int, bool) {
	q := r.URL.Query()
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.SessionListRequest{}, 0, false
	}
	if q.Get("since_ms") == "" {
		sinceMs = time.Now().Add(-defaultSessionListWindow).UnixMilli()
	}
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return wire.SessionListRequest{}, 0, false
	}
	return wire.SessionListRequest{
		SinceMs:           sinceMs,
		Cwd:               q.Get("cwd"),
		Limit:             limit,
		Cursor:            wire.Cursor(q.Get("cursor")),
		SourceAgent:       q.Get("source_agent"),
		Project:           q.Get("project"),
		ToolName:          q.Get("tool_name"),
		SkillName:         q.Get("skill_name"),
		FilePathSubstring: q.Get("file_path_substring"),
		WithFailures:      q.Get("with_failures") == "true",
		WithoutSummary:    q.Get("without_summary") == "true",
	}, offset, true
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
		s.storeError(w, "LoadSessionDigest", err)
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
	writeJSON(w, http.StatusOK, wire.ResolveSessionResponse{ID: id})
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
		s.storeError(w, "LoadCandidatePriorSessions", err)
		return
	}

	out := wire.CandidateSessionListResponse{
		Candidates: make([]wire.CandidateSession, 0, len(rows)),
	}
	for _, c := range rows {
		out.Candidates = append(out.Candidates, wire.CandidateSession{
			ID:          c.ID,
			Cwd:         c.Cwd,
			StartedAtMs: c.StartedAtMs,
			EndedAtMs:   c.EndedAtMs,
			Topic:       c.Topic,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionsSourceAgents serves GET /v1/sessions/source-agents.
// Distinct source_agent values from the sessions table, alphabetised.
// Empty result returns 200 with an empty array — fresh DB.
func (s *Server) handleSessionsSourceAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := store.LoadDistinctSourceAgents(r.Context(), s.store.DB())
	if err != nil {
		s.storeError(w, "LoadDistinctSourceAgents", err)
		return
	}
	if agents == nil {
		agents = []string{}
	}
	writeJSON(w, http.StatusOK, wire.SourceAgentsResponse{SourceAgents: agents})
}

func sessionDigestRowToWire(row store.SessionDigestRow) wire.SessionDigest {
	return wire.SessionDigest{
		ID:              row.ID,
		StartedAtMs:     row.StartedAtMs,
		EndedAtMs:       row.EndedAtMs,
		Cwd:             row.Cwd,
		StartCwd:        row.StartCwd,
		FirstPrompt:     row.FirstPrompt,
		LatestSummary:   row.LatestSummary,
		EventCount:      row.EventCount,
		SourceAgent:     row.SourceAgent,
		SourceSessionID: row.SourceSessionID,
	}
}
