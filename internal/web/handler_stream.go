package web

import (
	"fmt"
	"html"
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
			now := time.Now()
			for _, e := range events {
				// Each ingested event becomes TWO SSE frames:
				//
				//   event: event           — the live-feed row, listened
				//                           for by <ul id="livefeed">.
				//   event: session-<id>   — the per-row "Latest event"
				//                           cell + an OOB span that
				//                           updates the status dot.
				//
				// One ingest_seq, two frames; cursor advances once.
				// The bandwidth cost (one extra ~few-hundred-byte
				// frame per event) is the price of letting the
				// existing live feed AND the per-row cells share a
				// single SSE connection.
				if err := writeLiveFeedFrame(w, e); err != nil {
					return
				}
				if err := writeSessionFrame(w, e, now); err != nil {
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

// writeLiveFeedFrame serialises one LiveEvent as the SSE message
// the live feed listens for (`event: event`). Payload is the
// `<li class="livefeed-row">…</li>` fragment that prepends into
// `<ul id="livefeed">`. The `id:` line lets the browser's
// EventSource send a Last-Event-ID header on reconnect, matching
// the ?since_seq resume parameter.
func writeLiveFeedFrame(w http.ResponseWriter, e store.LiveEvent) error {
	frag := renderLiveEventFragment(e)
	_, err := fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n",
		e.IngestSeq, frag)
	return err
}

// writeSessionFrame serialises one LiveEvent as the SSE message
// the per-row "Latest event" cell listens for (`event: session-<id>`).
//
// The `data:` line carries the cell's new innerHTML AND an OOB
// status-dot <span> with hx-swap-oob="true". htmx processes the
// OOB span first (swapping the status dot by id) then swaps the
// rest of the response into the listening cell — one SSE frame,
// two cells updated.
//
// Single line on purpose: an SSE `data:` value spanning multiple
// lines is concatenated by the browser, but htmx's parser is
// happier with one line, and our renderers already flatten newlines
// from snippets via truncateForStream.
func writeSessionFrame(w http.ResponseWriter, e store.LiveEvent, now time.Time) error {
	cell := renderLatestEventCell(e)
	status, title := sessionStatus(&e, now)
	dot := renderStatusDot(e.SessionID, status, title, true /* OOB */)
	_, err := fmt.Fprintf(w, "id: %d\nevent: session-%s\ndata: %s%s\n\n",
		e.IngestSeq, e.SessionID, cell, dot)
	return err
}

// renderLiveEventFragment produces the one-line HTML row that
// appears in the live feed. Output is escaped via html.EscapeString
// at every interpolation point so neither the snippet nor the cwd
// can break out of the markup. Kept inline (not a template) so the
// SSE hot path doesn't pay template-lookup cost per event.
func renderLiveEventFragment(e store.LiveEvent) string {
	short := shortID(e.SessionID)
	cwd := "-"
	if e.Cwd.Valid && e.Cwd.String != "" {
		cwd = e.Cwd.String
	}
	snippet := truncateForStream(e.Snippet.String)
	return `<li class="livefeed-row">` +
		`<span class="ts">` + html.EscapeString(time.UnixMilli(e.TsSourceMs).UTC().Format("15:04:05")) + `</span> ` +
		`<span class="badge">` + html.EscapeString(e.Kind) + `</span> ` +
		`<a class="sid" href="/sessions/` + html.EscapeString(e.SessionID) + `">` + html.EscapeString(short) + `</a> ` +
		`<span class="cwd">` + html.EscapeString(cwd) + `</span> ` +
		`<span class="snippet">` + html.EscapeString(snippet) + `</span>` +
		`</li>`
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
