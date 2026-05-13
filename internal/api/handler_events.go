package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleEventsList serves GET /v1/events.
//
// Query params (all optional):
//
//   - session_id: filter to one session (empty = all sessions)
//   - since_seq:  exclusive cursor on raw_envelopes.ingest_seq;
//     events with ingest_seq > since_seq are returned
//   - limit:      page size, capped at wire.MaxPageLimit
//
// Response body is an wire.EventListResponse — a slice of
// wire.Event plus the current LatestSeq watermark.
func (s *Server) handleEventsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sessionID := q.Get("session_id")

	sinceSeq, ok := parseInt64Query(w, r, "since_seq")
	if !ok {
		return
	}

	limit, ok := parseLimitQuery(w, r, wire.DefaultPageLimit)
	if !ok {
		return
	}

	rows, err := store.LoadEventsSinceSeq(r.Context(), s.store.DB(), sinceSeq, sessionID, limit)
	if err != nil {
		s.slog.Error("LoadEventsSinceSeq", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	latest, err := store.LatestIngestSeq(r.Context(), s.store.DB())
	if err != nil {
		s.slog.Error("LatestIngestSeq", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	out := wire.EventListResponse{
		Events:    make([]wire.Event, 0, len(rows)),
		LatestSeq: latest,
	}
	for _, e := range rows {
		out.Events = append(out.Events, wire.Event{
			IngestSeq:  e.IngestSeq,
			EventID:    e.EventID,
			SessionID:  e.SessionID,
			Kind:       e.Kind,
			TsSourceMs: e.TsSourceMs,
			TsServerMs: e.TsServerMs,
			Cwd:        e.Cwd,
			Snippet:    e.Snippet,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
