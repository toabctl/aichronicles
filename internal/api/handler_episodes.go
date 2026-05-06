package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
	"github.com/toabctl/aichronicles/pkg/events"
)

// handleEpisodesList serves GET /v1/episodes with optional
// session_id / cwd / query_contains / since_ms / limit filters.
// Backed by store.FindEpisodes; an empty filter set returns the
// most recent DefaultFindEpisodesLimit episodes overall.
func (s *Server) handleEpisodesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := store.FindEpisodesOpts{
		SessionID:     q.Get("session_id"),
		Cwd:           q.Get("cwd"),
		QueryContains: q.Get("query_contains"),
	}
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	opts.SinceMs = sinceMs
	limit, ok := parseLimitQuery(w, r, 0)
	if !ok {
		return
	}
	opts.Limit = limit

	rows, err := store.FindEpisodes(r.Context(), s.store.DB(), opts)
	if err != nil {
		s.slog.Error("FindEpisodes", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	out := api.EpisodeListResponse{Episodes: make([]api.Episode, 0, len(rows))}
	for _, e := range rows {
		out.Episodes = append(out.Episodes, episodeToWire(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func episodeToWire(e events.Episode) api.Episode {
	return api.Episode{
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
