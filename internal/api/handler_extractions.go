package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleExtractions serves GET /v1/extractions?kind=&value=&since_ms=,
// the reverse of handleSessionExtractions: which sessions produced a
// value, e.g. which session created a PR (kind=pr_created). kind is
// required; value is optional and matched exactly; pagination follows
// the shared limit/cursor contract. Backed by store.FindExtractions.
func (s *Server) handleExtractions(w http.ResponseWriter, r *http.Request) {
	req, offset, ok := parseExtractionListRequest(w, r)
	if !ok {
		return
	}
	rows, err := store.FindExtractions(r.Context(), s.store.DB(), store.FindExtractionsOpts{
		Kind:    req.Kind,
		Value:   req.Value,
		SinceMs: req.SinceMs,
		Limit:   req.Limit,
		Offset:  offset,
	})
	if err != nil {
		s.storeError(w, "FindExtractions", err)
		return
	}
	out := wire.ExtractionListResponse{Extractions: make([]wire.ExtractionSighting, 0, len(rows))}
	for _, x := range rows {
		out.Extractions = append(out.Extractions, wire.ExtractionSighting{
			SessionID:  x.SessionID,
			Kind:       x.Kind,
			Value:      x.Value,
			TsSourceMs: x.TsSourceMs,
			Cwd:        x.Cwd,
		})
	}
	out.NextCursor = nextCursor(offset, req.Limit, len(rows))
	writeJSON(w, http.StatusOK, out)
}

// parseExtractionListRequest decodes + validates the GET /v1/extractions
// query into wire.ExtractionListRequest (server mirror of
// apiclient.Client.Extractions). Returns the request, the decoded page
// offset, and ok=false after a 400.
func parseExtractionListRequest(w http.ResponseWriter, r *http.Request) (wire.ExtractionListRequest, int, bool) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		writeProblem(w, http.StatusBadRequest, "Missing kind", "kind query param is required")
		return wire.ExtractionListRequest{}, 0, false
	}
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.ExtractionListRequest{}, 0, false
	}
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return wire.ExtractionListRequest{}, 0, false
	}
	return wire.ExtractionListRequest{
		Kind:    kind,
		Value:   q.Get("value"),
		SinceMs: sinceMs,
		Limit:   limit,
		Cursor:  wire.Cursor(q.Get("cursor")),
	}, offset, true
}
