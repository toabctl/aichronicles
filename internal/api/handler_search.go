package api

import (
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
)

// handleSearch serves GET /v1/search.
//
// Q is required (400 when missing). All other params are
// optional filters AND'd together; the underlying SearchEvents
// handles the FTS5 query parsing and snippet generation.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	query := q.Get("q")
	if query == "" {
		writeProblem(w, http.StatusBadRequest, "Missing q", "search query is required")
		return
	}

	opts := store.SearchEventOpts{
		Query:             query,
		Kind:              q.Get("kind"),
		SessionID:         q.Get("session_id"),
		SubagentID:        q.Get("subagent_id"),
		SourceAgent:       q.Get("source_agent"),
		ToolName:          q.Get("tool_name"),
		SkillName:         q.Get("skill_name"),
		FilePathSubstring: q.Get("file_path_substring"),
		WithFailures:      q.Get("with_failures") == "true",
	}
	if v := q.Get("since_ms"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid since_ms", "")
			return
		}
		opts.SinceMs = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", "")
			return
		}
		if n > api.MaxPageLimit {
			n = api.MaxPageLimit
		}
		opts.Limit = n
	}

	hits, err := store.SearchEvents(r.Context(), s.store.DB(), opts)
	if err != nil {
		s.slog.Error("SearchEvents", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := api.SearchResponse{Hits: make([]api.SearchHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, api.SearchHit{
			SessionID:  h.SessionID,
			Kind:       h.Kind,
			Cwd:        sqlNullToPtr(h.Cwd),
			TsSourceMs: h.TsSourceMs,
			Content:    sqlNullToPtr(h.Content),
			Snippet:    sqlNullToPtr(h.Snippet),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
