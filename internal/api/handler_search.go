package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

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
	req, ftsQuery, ok := parseSearchRequest(w, r)
	if !ok {
		return
	}

	opts := store.SearchEventOpts{
		Query:             ftsQuery,
		Kind:              req.Kind,
		SessionID:         req.SessionID,
		SubagentID:        req.SubagentID,
		SourceAgent:       req.SourceAgent,
		ToolName:          req.ToolName,
		SkillName:         req.SkillName,
		FilePathSubstring: req.FilePathSubstring,
		WithFailures:      req.WithFailures,
		NoDedup:           req.NoDedup,
		SinceMs:           req.SinceMs,
		Limit:             req.Limit,
	}

	// A cursor pins the page position plus everything that must stay
	// constant across pages (FTS stage, as-of now-ms, order, dedup) —
	// it is authoritative over the re-sent order/dedup so the locks
	// can't be broken mid-pagination. No cursor → first page: pin
	// now-ms here so the value we put in NextCursor matches what the
	// store scores against.
	if req.Cursor != "" {
		cur, err := wire.DecodeSearchCursor(req.Cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid cursor", err.Error())
			return
		}
		opts.Offset = cur.Off
		opts.Stage = cur.Stage
		opts.NowMs = cur.Now
		opts.Order = store.SearchOrder(cur.Ord)
		opts.NoDedup = cur.Dedup
	} else {
		opts.NowMs = time.Now().UnixMilli()
	}

	// Resolve the effective page size the store will apply, so the
	// NextCursor "last page" test (len(hits) < limit) compares against
	// the right number, and bound the offset depth.
	if opts.Limit <= 0 {
		opts.Limit = store.DefaultSearchLimit
	}
	if opts.Offset+opts.Limit > wire.MaxOffset {
		writeProblem(w, http.StatusBadRequest, "Offset too deep",
			fmt.Sprintf("search pagination is limited to %d results", wire.MaxOffset))
		return
	}

	hits, stage, err := store.SearchEventsPaged(r.Context(), s.store.DB(), opts)
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

	// A short page is the last page (the single stop signal). A full
	// page emits a cursor for the next; following it may return zero
	// rows and an empty cursor, which is correct.
	if len(hits) == opts.Limit {
		next, err := wire.EncodeSearchCursor(wire.SearchCursor{
			Off:   opts.Offset + len(hits),
			Stage: stage,
			Now:   opts.NowMs,
			Ord:   int(opts.Order),
			Dedup: opts.NoDedup,
		})
		if err != nil {
			s.storeError(w, "EncodeSearchCursor", err)
			return
		}
		out.NextCursor = next
	}
	writeJSON(w, http.StatusOK, out)
}

// parseSearchRequest decodes + validates the GET /v1/search query into
// the canonical wire.SearchRequest (server mirror of
// apiclient.Client.Search). q is required and must parse as FTS5 — a
// 400 with the searchquery diagnostic is returned otherwise. Also
// returns the FTS5-translated query the store expects, so the handler
// doesn't re-parse: wire carries the raw q, the store wants FTS5.
func parseSearchRequest(w http.ResponseWriter, r *http.Request) (wire.SearchRequest, string, bool) {
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		writeProblem(w, http.StatusBadRequest, "Missing q", "search query is required")
		return wire.SearchRequest{}, "", false
	}
	// store.SearchEvents expects an FTS5-syntax query; the user-facing
	// form is plain words + optional "quoted phrases". Parse here so
	// every consumer sees ErrSyntax as a clean 400 instead of a
	// generic 500 from SQLite.
	ftsQuery, err := searchquery.ToFTS5(query)
	if err != nil {
		switch {
		case errors.Is(err, searchquery.ErrEmpty):
			writeProblem(w, http.StatusBadRequest, "Missing q", "search query is required")
		default:
			writeProblem(w, http.StatusBadRequest, "Invalid q", err.Error())
		}
		return wire.SearchRequest{}, "", false
	}
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.SearchRequest{}, "", false
	}
	limit, ok := parseLimitQuery(w, r, 0)
	if !ok {
		return wire.SearchRequest{}, "", false
	}
	return wire.SearchRequest{
		Q:                 query,
		Kind:              q.Get("kind"),
		SessionID:         q.Get("session_id"),
		SubagentID:        q.Get("subagent_id"),
		SourceAgent:       q.Get("source_agent"),
		ToolName:          q.Get("tool_name"),
		SkillName:         q.Get("skill_name"),
		FilePathSubstring: q.Get("file_path_substring"),
		SinceMs:           sinceMs,
		WithFailures:      q.Get("with_failures") == "true",
		NoDedup:           q.Get("no_dedup") == "true",
		Limit:             limit,
		Cursor:            wire.Cursor(q.Get("cursor")),
	}, ftsQuery, true
}
