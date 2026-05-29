package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleEpisodesList serves GET /v1/episodes with optional
// session_id / cwd / query_contains / since_ms / limit filters.
// Backed by store.FindEpisodes; an empty filter set returns the
// most recent DefaultFindEpisodesLimit episodes overall.
func (s *Server) handleEpisodesList(w http.ResponseWriter, r *http.Request) {
	req, offset, ok := parseEpisodeListRequest(w, r)
	if !ok {
		return
	}
	opts := store.FindEpisodesOpts{
		SessionID:     req.SessionID,
		Cwd:           req.Cwd,
		QueryContains: req.QueryContains,
		SinceMs:       req.SinceMs,
		Limit:         req.Limit,
		Offset:        offset,
	}

	rows, err := store.FindEpisodes(r.Context(), s.store.DB(), opts)
	if err != nil {
		s.storeError(w, "FindEpisodes", err)
		return
	}
	out := wire.EpisodeListResponse{Episodes: make([]wire.Episode, 0, len(rows))}
	for _, e := range rows {
		out.Episodes = append(out.Episodes, episodeToWire(e))
	}
	out.NextCursor = nextCursor(offset, req.Limit, len(rows))
	writeJSON(w, http.StatusOK, out)
}

// parseEpisodeListRequest decodes + validates the GET /v1/episodes
// query into wire.EpisodeListRequest (server mirror of
// apiclient.Client.Episodes). Returns the request, the decoded page
// offset, and ok=false after a 400.
func parseEpisodeListRequest(w http.ResponseWriter, r *http.Request) (wire.EpisodeListRequest, int, bool) {
	q := r.URL.Query()
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return wire.EpisodeListRequest{}, 0, false
	}
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return wire.EpisodeListRequest{}, 0, false
	}
	return wire.EpisodeListRequest{
		SessionID:     q.Get("session_id"),
		Cwd:           q.Get("cwd"),
		QueryContains: q.Get("query_contains"),
		SinceMs:       sinceMs,
		Limit:         limit,
		Cursor:        wire.Cursor(q.Get("cursor")),
	}, offset, true
}

func episodeToWire(e events.Episode) wire.Episode {
	return wire.Episode{
		ID:            e.ID,
		SessionID:     e.SessionID,
		Ordinal:       e.Ordinal,
		StartedAtMs:   e.StartedAtMs,
		EndedAtMs:     e.EndedAtMs,
		Cwd:           e.Cwd.Ptr(),
		IntentSummary: e.IntentSummary,
		EventCount:    e.EventCount,
		FirstEventID:  e.FirstEventID,
	}
}
