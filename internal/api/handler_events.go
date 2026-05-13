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
		s.storeError(w, "LoadEventsSinceSeq", err)
		return
	}

	latest, err := store.LatestIngestSeq(r.Context(), s.store.DB())
	if err != nil {
		s.storeError(w, "LatestIngestSeq", err)
		return
	}

	out := wire.EventListResponse{
		Events:    make([]wire.Event, 0, len(rows)),
		LatestSeq: latest,
	}
	for _, e := range rows {
		out.Events = append(out.Events, liveEventToWire(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEventsLatestBatch serves GET /v1/events/latest?session_ids=id1,id2.
// Returns a {session_id → Event} map carrying each session's most
// recent event. Sessions with no events are omitted. Used by the
// web's session-list page to render "latest activity" per row in
// one round-trip; replaces an N-query loop.
func (s *Server) handleEventsLatestBatch(w http.ResponseWriter, r *http.Request) {
	ids := parseSessionIDsQuery(r.URL.Query().Get("session_ids"))
	if len(ids) == 0 {
		writeProblem(w, http.StatusBadRequest, "Missing session_ids",
			"session_ids is a comma-separated list of ids")
		return
	}
	rows, err := store.LoadLatestEventsIndexedByID(r.Context(), s.store.DB(), ids)
	if err != nil {
		s.storeError(w, "LoadLatestEventsIndexedByID", err)
		return
	}
	out := wire.LatestEventsBatchResponse{Events: make(map[string]wire.Event, len(rows))}
	for id, e := range rows {
		out.Events[id] = liveEventToWire(e)
	}
	writeJSON(w, http.StatusOK, out)
}

func liveEventToWire(e store.LiveEvent) wire.Event {
	return wire.Event{
		IngestSeq:  e.IngestSeq,
		EventID:    e.EventID,
		SessionID:  e.SessionID,
		Kind:       e.Kind,
		TsSourceMs: e.TsSourceMs,
		TsServerMs: e.TsServerMs,
		Cwd:        e.Cwd,
		Snippet:    e.Snippet,
	}
}
