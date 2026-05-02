package api

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
)

// handleEventsList serves GET /v1/events.
//
// Query params (all optional):
//
//   - session_id: filter to one session (empty = all sessions)
//   - since_seq:  exclusive cursor on raw_envelopes.ingest_seq;
//     events with ingest_seq > since_seq are returned
//   - limit:      page size, capped at api.MaxPageLimit
//
// Response body is an api.EventListResponse — a slice of
// api.Event plus the current LatestSeq watermark.
func (s *Server) handleEventsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sessionID := q.Get("session_id")

	var sinceSeq int64
	if v := q.Get("since_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid since_seq", err.Error())
			return
		}
		if n < 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid since_seq", "must be non-negative")
			return
		}
		sinceSeq = n
	}

	limit := api.DefaultPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", err.Error())
			return
		}
		if n <= 0 {
			writeProblem(w, http.StatusBadRequest, "Invalid limit", "must be positive")
			return
		}
		if n > api.MaxPageLimit {
			n = api.MaxPageLimit
		}
		limit = n
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

	out := api.EventListResponse{
		Events:    make([]api.Event, 0, len(rows)),
		LatestSeq: latest,
	}
	for _, e := range rows {
		out.Events = append(out.Events, api.Event{
			IngestSeq:  e.IngestSeq,
			EventID:    e.EventID,
			SessionID:  e.SessionID,
			Kind:       e.Kind,
			TsSourceMs: e.TsSourceMs,
			TsServerMs: e.TsServerMs,
			Cwd:        sqlNullToPtr(e.Cwd),
			Snippet:    sqlNullToPtr(e.Snippet),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// sqlNullToPtr translates a sql.NullString to the wire-clean
// *string shape pkg/api types use. Returns nil when the column
// was NULL, otherwise a pointer to the value. Centralized here
// so every handler that walks internal/store row types projects
// nullable text columns identically.
func sqlNullToPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}
