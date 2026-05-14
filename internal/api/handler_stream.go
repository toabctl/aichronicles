package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// streamHeartbeatInterval keeps the connection alive across
// reverse proxies and browser idle timeouts. 15s is well below
// the typical 30s minimum.
const streamHeartbeatInterval = 15 * time.Second

// streamReplayLimit caps how many events we'll replay on a resume
// request. A client that's been disconnected for so long that >N
// events accumulated should reload from /v1/events instead of
// trickling them through SSE; the cap also bounds the worst-case
// memory + work per subscriber. Generous enough that a brief proxy
// hiccup never trips it.
const streamReplayLimit = 1000

// handleStream serves GET /v1/stream as JSON-bodied Server-Sent
// Events. One goroutine per connection, fed by the in-process
// sseBus.
//
// Frame shape:
//
//	id: <ingest_seq>
//	event: event
//	data: <wire.StreamEvent JSON>
//
// Heartbeats are SSE comment lines (`:keepalive ...`) every
// streamHeartbeatInterval to keep middleboxes from severing the
// connection during quiet periods.
//
// Backpressure: a slow consumer is dropped from the bus (the
// next read returns the zero value). The handler exits cleanly
// in that case.
//
// Capacity: returns 429 when SSEMaxSubscribers is reached.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError,
			"streaming unsupported", "ResponseWriter does not implement http.Flusher")
		return
	}

	// Determine the resume cursor. Per the SSE spec, the
	// Last-Event-ID request header carries the id: of the last
	// frame the client received; we also accept a `since_seq` query
	// param so non-browser clients (and tests) can opt in without
	// the EventSource ceremony. The query param wins when both are
	// present so an explicit retry can override a stale browser
	// header.
	sinceSeq := parseStreamResume(r)

	ch, cancel, ok := s.sseBus.subscribe()
	if !ok {
		writeProblem(w, http.StatusTooManyRequests,
			"too many streaming subscribers",
			"server-wide cap reached; try again shortly")
		return
	}
	defer cancel()

	// Disable the per-request WriteTimeout for the lifetime of
	// this SSE connection; the server-wide bound is right for
	// regular requests but would sever live streams. r.Context()
	// + the SSEMaxSubscribers cap remain the lifecycle bounds.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Replay the gap between sinceSeq and the live bus. We
	// subscribed BEFORE the DB read so any event landing in between
	// is captured by the bus channel; the dedup-by-maxReplayed gate
	// below drops the duplicate when we resume normal bus
	// consumption.
	var maxReplayed int64
	if sinceSeq > 0 {
		replayed, err := store.LoadStreamEventsSinceSeq(r.Context(), s.store.DB(), sinceSeq, streamReplayLimit)
		if err != nil {
			s.slog.Warn("stream: replay query failed", "since_seq", sinceSeq, "err", err)
		}
		for _, ev := range replayed {
			if err := writeStreamEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
			if ev.IngestSeq > maxReplayed {
				maxReplayed = ev.IngestSeq
			}
		}
	}

	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ":keepalive %d\n\n", time.Now().Unix()); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				// Bus closed our subscription (overflow or
				// server shutdown). Exit cleanly.
				return
			}
			// Drop events the replay already emitted. The race we
			// guard against: an event landing between subscribe()
			// and the DB read appears in BOTH the bus channel and
			// the replay batch.
			if maxReplayed > 0 && ev.IngestSeq > 0 && ev.IngestSeq <= maxReplayed {
				continue
			}
			if err := writeStreamEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseStreamResume extracts the resume cursor from the SSE
// Last-Event-ID header and/or the `since_seq` query param. Returns
// 0 (no resume) for missing or malformed input — the gate is a
// strict ">0" so anything else means "stream live only".
func parseStreamResume(r *http.Request) int64 {
	var sinceSeq int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			sinceSeq = n
		}
	}
	if v := r.URL.Query().Get("since_seq"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			sinceSeq = n
		}
	}
	return sinceSeq
}

// writeStreamEvent writes one SSE frame carrying a JSON-encoded
// StreamEvent. The id: line uses ingest_seq so a reconnecting
// client's Last-Event-ID header can be parsed straight back into
// the cursor.
func writeStreamEvent(w http.ResponseWriter, ev wire.StreamEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		// Should never fail for our struct; failsafe so a future
		// non-marshalable field doesn't crash the goroutine.
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n",
		ev.IngestSeq, body)
	return err
}
