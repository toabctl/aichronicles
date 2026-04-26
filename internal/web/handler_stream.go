package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

const (
	// streamPollInterval is how often the SSE handler polls the
	// store for new events. 500ms is the sweet spot: the user
	// perceives "live" updates inside human reaction time, and
	// the indexed query is microseconds against an idle DB.
	streamPollInterval = 500 * time.Millisecond

	// streamHeartbeat keeps the connection alive across reverse
	// proxies and browser idle timeouts. 15s is well below the
	// typical 30s minimum.
	streamHeartbeat = 15 * time.Second

	// streamMaxConcurrent caps simultaneous SSE connections.
	// Each connection spends one Go goroutine and one SQL query
	// per poll — 20 is plenty for a personal-use tool and
	// bounds the resource cost so a runaway client (or a forgot-
	// to-close-tabs day) can't pin the daemon.
	streamMaxConcurrent = 20

	// streamBatchLimit caps events per poll so a backlog doesn't
	// produce a single oversized SSE write that could trip a
	// frontend's frame budget. Subsequent polls drain remaining
	// events at the same cadence.
	streamBatchLimit = 50

	// streamSnippetMaxRunes is how much of content_text the
	// stream embeds per event. Tight enough that 50 events fit
	// in a single ~10KB SSE write; loose enough to render a
	// useful preview inline.
	streamSnippetMaxRunes = 200
)

// streamEvent is the JSON payload for one SSE message. The shape
// matches what the frontend list templates render so an htmx
// SSE-swap can substitute it directly.
type streamEvent struct {
	IngestSeq  int64  `json:"ingest_seq"`
	EventID    string `json:"event_id"`
	SessionID  string `json:"session_id"`
	ShortID    string `json:"short_id"`
	Kind       string `json:"kind"`
	TsSourceMs int64  `json:"ts_source_ms"`
	Cwd        string `json:"cwd,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
}

// streamHandler serves Server-Sent Events with the latest event
// stream. One goroutine per connection, polling every
// streamPollInterval. Heartbeat keeps the channel open during
// idle. Cleanup tied to r.Context().Done().
func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request) {
	if s.streamCount.Load() >= streamMaxConcurrent {
		// Refuse rather than serve a degraded experience.
		// Returning 429 lets the htmx ext / EventSource back
		// off gracefully via its built-in reconnect backoff.
		http.Error(w, "too many streaming connections; try again soon",
			http.StatusTooManyRequests)
		return
	}
	s.streamCount.Add(1)
	defer s.streamCount.Add(-1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Should never happen with net/http's default handler;
		// fail loud so the test catches a misconfiguration.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering when present
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sessionFilter := r.URL.Query().Get("session_id")

	// Initial cursor: the latest ingest_seq at connect time, so
	// a new tab doesn't get the entire historical event stream
	// fired at it. Clients that want backfill ask for it via
	// the regular paginated endpoints.
	cursor, err := store.LatestIngestSeq(r.Context(), s.store.DB())
	if err != nil {
		s.log.Error("stream: load latest seq", "err", err)
		return
	}
	// Clients can ask to resume from a specific seq via ?since_seq=
	// — useful when the connection drops and we want to catch up
	// without sending all history.
	if v := r.URL.Query().Get("since_seq"); v != "" {
		if n, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil && n >= 0 {
			cursor = n
		}
	}

	pollTicker := time.NewTicker(streamPollInterval)
	defer pollTicker.Stop()
	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// SSE comment line — keeps the connection warm
			// without delivering an event payload to the
			// client. The leading `:` makes it a comment per
			// the SSE spec.
			if _, err := fmt.Fprintf(w, ":keepalive %d\n\n", time.Now().Unix()); err != nil {
				return
			}
			flusher.Flush()
		case <-pollTicker.C:
			events, err := store.LoadEventsSinceSeq(
				r.Context(), s.store.DB(), cursor, sessionFilter, streamBatchLimit,
			)
			if err != nil {
				s.log.Error("stream: poll events", "err", err)
				// Soft failure — try again next tick rather
				// than dropping the client. Hard errors
				// (ctx done) come through the other branch.
				continue
			}
			for _, e := range events {
				if err := writeStreamEvent(w, e); err != nil {
					return
				}
				cursor = e.IngestSeq
			}
			if len(events) > 0 {
				flusher.Flush()
			}
		}
	}
}

// writeStreamEvent serialises one LiveEvent as an SSE message.
// The `id:` line lets the browser's EventSource send a
// Last-Event-ID header on reconnect, matching the ?since_seq
// resume parameter — no events are missed across a disconnect.
func writeStreamEvent(w http.ResponseWriter, e store.LiveEvent) error {
	payload := streamEvent{
		IngestSeq:  e.IngestSeq,
		EventID:    e.EventID,
		SessionID:  e.SessionID,
		ShortID:    shortID(e.SessionID),
		Kind:       e.Kind,
		TsSourceMs: e.TsSourceMs,
		Cwd:        e.Cwd.String,
		Snippet:    truncateForStream(e.Snippet.String),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal stream event: %w", err)
	}
	// SSE frame: id (for resume), event (for hx-ext sse-swap
	// routing), data (one line). End with double-newline.
	_, err = fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n",
		e.IngestSeq, body)
	return err
}

// truncateForStream flattens whitespace and rune-caps the snippet
// to keep one SSE write under the frontend's parse budget.
func truncateForStream(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range "\n\r\t" {
		s = strings.ReplaceAll(s, string(r), " ")
	}
	runes := []rune(s)
	if len(runes) > streamSnippetMaxRunes {
		return string(runes[:streamSnippetMaxRunes]) + "…"
	}
	return s
}
