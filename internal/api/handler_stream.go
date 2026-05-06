package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/wire"
)

// streamHeartbeatInterval keeps the connection alive across
// reverse proxies and browser idle timeouts. 15s is well below
// the typical 30s minimum.
const streamHeartbeatInterval = 15 * time.Second

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
			if err := writeStreamEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
