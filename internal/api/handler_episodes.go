package api

import (
	"net/http"
	"strconv"

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
	var cwd *string
	if e.Cwd.Valid {
		v := e.Cwd.String
		cwd = &v
	}
	return api.Episode{
		ID:            e.ID,
		SessionID:     e.SessionID,
		Ordinal:       e.Ordinal,
		StartedAtMs:   e.StartedAtMs,
		EndedAtMs:     e.EndedAtMs,
		Cwd:           cwd,
		IntentSummary: e.IntentSummary,
		EventCount:    e.EventCount,
		FirstEventID:  e.FirstEventID,
	}
}
