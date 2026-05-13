package api

import (
	"errors"
	"net/http"

	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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

	// store.SearchEvents expects an FTS5-syntax query; the user-
	// facing form is plain words + optional "quoted phrases".
	// Parse here so every consumer (CLI, MCP, web, third party)
	// gets the same shape on the wire and sees ErrSyntax as a
	// clean 400 instead of a generic 500 from SQLite.
	ftsQuery, err := searchquery.ToFTS5(query)
	if err != nil {
		switch {
		case errors.Is(err, searchquery.ErrEmpty):
			writeProblem(w, http.StatusBadRequest, "Missing q", "search query is required")
		case errors.Is(err, searchquery.ErrSyntax):
			writeProblem(w, http.StatusBadRequest, "Invalid q", err.Error())
		default:
			writeProblem(w, http.StatusBadRequest, "Invalid q", err.Error())
		}
		return
	}

	opts := store.SearchEventOpts{
		Query:             ftsQuery,
		Kind:              q.Get("kind"),
		SessionID:         q.Get("session_id"),
		SubagentID:        q.Get("subagent_id"),
		SourceAgent:       q.Get("source_agent"),
		ToolName:          q.Get("tool_name"),
		SkillName:         q.Get("skill_name"),
		FilePathSubstring: q.Get("file_path_substring"),
		WithFailures:      q.Get("with_failures") == "true",
		NoDedup:           q.Get("no_dedup") == "true",
	}
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	opts.SinceMs = sinceMs
	if opts.Limit, ok = parseLimitQuery(w, r, 0); !ok {
		return
	}

	hits, err := store.SearchEvents(r.Context(), s.store.DB(), opts)
	if err != nil {
		s.storeError(w, "SearchEvents", err)
		return
	}

	out := wire.SearchResponse{Hits: make([]wire.SearchHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, wire.SearchHit{
			SessionID:  h.SessionID,
			Kind:       h.Kind,
			Cwd:        h.Cwd,
			TsSourceMs: h.TsSourceMs,
			Content:    h.Content,
			Snippet:    h.Snippet,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
