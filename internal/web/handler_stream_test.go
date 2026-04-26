package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// sseFrame is one parsed SSE message — event name + data payload.
// id is captured too because the cursor-resume path (?since_seq=)
// depends on it being attached to every frame.
type sseFrame struct {
	Event string
	Data  string
	ID    string
}

// readSSEFrames is the legacy helper returning just `data:` payloads.
// New tests should prefer readSSEEnvelopes which also surfaces the
// event name (so tests can distinguish live-feed frames from
// per-session frames).
func readSSEFrames(t *testing.T, ctx context.Context, r *http.Response, n int) []string {
	t.Helper()
	envs := readSSEEnvelopes(t, ctx, r, n)
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.Data
	}
	return out
}

// readSSEEnvelopes reads SSE-formatted bytes from r until ctx is
// done or n complete frames have arrived. Each frame may consist
// of an `id:` line, an `event:` line, one or more `data:` lines,
// terminated by a blank line. Returns the parsed envelopes.
func readSSEEnvelopes(t *testing.T, ctx context.Context, r *http.Response, n int) []sseFrame {
	t.Helper()
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	var (
		frames []sseFrame
		cur    sseFrame
	)
	doneCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = r.Body.Close()
		close(doneCh)
	}()

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			cur.Data += strings.TrimPrefix(line, "data:")
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "id:"):
			cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case line == "":
			// Frame terminator. Comments (":keepalive…") have empty
			// data and event; only emit when data is non-empty.
			if cur.Data != "" {
				cur.Data = strings.TrimSpace(cur.Data)
				frames = append(frames, cur)
				if len(frames) >= n {
					return frames
				}
				cur = sseFrame{}
			}
		}
		select {
		case <-doneCh:
			return frames
		default:
		}
	}
	return frames
}

func TestStream_PushesNewEvents(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type: got %q, want text/event-stream*", ct)
	}

	// Give the handler a moment to install its cursor at
	// MAX(ingest_seq), then ingest a fresh event.
	time.Sleep(100 * time.Millisecond)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	seedSession(t, st, "sess-stream", "live event content", now)

	frames := readSSEFrames(t, ctx, resp, 1)
	if len(frames) == 0 {
		t.Fatalf("no SSE frames received")
	}
	frag := frames[0]
	for _, want := range []string{
		`class="livefeed-row"`,
		`>user_prompt<`,
		`live event content`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q:\n%s", want, frag)
		}
	}
}

func TestStream_RespectsSessionIDFilter(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	wantedID := seedSession(t, st, "sess-keep", "should pass filter", now)
	_ = seedSession(t, st, "sess-skip", "should be filtered out", now.Add(time.Second))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Connect AFTER seeding, with since_seq=0 so we get the
	// historical events too. Filter to wantedID: the second
	// event must not arrive on this stream.
	req, _ := http.NewRequestWithContext(ctx, "GET",
		base+"/stream?since_seq=0&session_id="+wantedID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frames := readSSEFrames(t, ctx, resp, 1)
	if len(frames) != 1 {
		t.Fatalf("expected exactly 1 frame for filtered session, got %d", len(frames))
	}
	if !strings.Contains(frames[0], wantedID) {
		t.Errorf("filter leaked: fragment didn't carry %s\n%s", wantedID, frames[0])
	}
	if strings.Contains(frames[0], "should be filtered out") {
		t.Errorf("filter leaked the unwanted snippet:\n%s", frames[0])
	}
}

func TestStream_HeartbeatKeepsConnectionAlive(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read raw bytes for ~1s; we should see polling activity but
	// no events. The point is that the connection stays open
	// (no EOF) — heartbeat fires every 15s in production but
	// the absence of a close is what matters here.
	buf := make([]byte, 4096)
	deadline := time.After(1 * time.Second)
	closed := make(chan struct{})
	go func() {
		_, _ = resp.Body.Read(buf)
		close(closed)
	}()
	select {
	case <-deadline:
		// good — connection still open
	case <-closed:
		t.Errorf("connection closed prematurely; body: %q", string(buf))
	}
}

func TestStream_ContextCancelDecrementsConnectionCount(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	// Open the cap's worth of connections, hold them, then
	// release them all and verify a fresh connection succeeds —
	// this proves the deferred Add(-1) actually fired in the
	// cancelled handler.
	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup
	for i := 0; i < streamMaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", base+"/stream", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			buf := make([]byte, 4096)
			for {
				if _, err := resp.Body.Read(buf); err != nil {
					return
				}
			}
		}()
	}

	// Let the cap fill, then release every holder.
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
	// Brief settle so the deferred decrements run.
	time.Sleep(100 * time.Millisecond)

	// A fresh connection must now be accepted. If the cancelled
	// goroutines hadn't decremented the counter we'd get 429.
	freshCtx, freshCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer freshCancel()
	req, _ := http.NewRequestWithContext(freshCtx, "GET", base+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fresh connection failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post-disconnect status: got %d, want 200 (connection counter leaked)",
			resp.StatusCode)
	}
}

func TestStream_EmitsLiveFeedAndPerSessionFrames(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Let the cursor settle, then ingest one event — should get TWO
	// SSE frames back: one named "event" (live feed) and one named
	// "session-<id>" (per-row latest-event cell + OOB status).
	time.Sleep(100 * time.Millisecond)
	id := seedSession(t, st, "sess-dual", "live event content", time.Now())

	frames := readSSEEnvelopes(t, ctx, resp, 2)
	if len(frames) < 2 {
		t.Fatalf("expected 2 frames per event, got %d", len(frames))
	}

	var live, sess *sseFrame
	for i := range frames {
		switch frames[i].Event {
		case "event":
			live = &frames[i]
		case "session-" + id:
			sess = &frames[i]
		}
	}
	if live == nil {
		t.Fatalf("missing live-feed frame (event: event):\n%+v", frames)
	}
	if sess == nil {
		t.Fatalf("missing session frame (event: session-%s):\n%+v", id, frames)
	}

	// Live-feed frame keeps the existing <li> shape.
	if !strings.Contains(live.Data, `class="livefeed-row"`) {
		t.Errorf("live frame missing livefeed-row markup:\n%s", live.Data)
	}
	// Per-session frame carries the latest-cell renderer's output
	// (kind badge + snippet) AND an OOB status span.
	for _, want := range []string{
		`<span class="badge">user_prompt</span>`,
		`live event content`,
		`id="status-` + id + `"`,
		`hx-swap-oob="true"`,
	} {
		if !strings.Contains(sess.Data, want) {
			t.Errorf("session frame missing %q:\n%s", want, sess.Data)
		}
	}

	// The id field is the ingest_seq — same on both frames since they
	// represent the same underlying event.
	if live.ID == "" || sess.ID == "" || live.ID != sess.ID {
		t.Errorf("expected matching non-empty id on both frames; got live=%q sess=%q",
			live.ID, sess.ID)
	}
}

func TestStream_PerSessionFrameStatusActiveForNormalEvent(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	id := seedSession(t, st, "sess-active-via-sse", "doesn't matter", time.Now())
	req, _ := http.NewRequestWithContext(ctx, "GET",
		base+"/stream?since_seq=0&session_id="+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frames := readSSEEnvelopes(t, ctx, resp, 2)
	var sess *sseFrame
	for i := range frames {
		if frames[i].Event == "session-"+id {
			sess = &frames[i]
			break
		}
	}
	if sess == nil {
		t.Fatalf("missing session frame:\n%+v", frames)
	}
	if !strings.Contains(sess.Data, "status-active") {
		t.Errorf("normal event should drive status-active; got:\n%s", sess.Data)
	}
}

func TestStream_PerSessionFrameStatusEndedForSessionEnd(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	time.Sleep(100 * time.Millisecond)

	// Ingest a session_end envelope — the OOB status span should
	// flip to status-ended.
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-finishing",
		Kind:            "session_end",
		Role:            "system",
		TsSource:        time.Now().UTC(),
		Cwd:             "/work/sess-finishing",
		Payload:         map[string]any{"reason": "user-quit"},
		Transport:       "hook",
		Redaction:       &ingest.Redaction{Applied: true},
	}
	rawBytes, _ := json.Marshal(env)
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.IngestEnvelope(t.Context(), tx, env, rawBytes, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest session_end: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	expectedID := ingest.DeriveSessionID("claude-code", "sess-finishing")

	frames := readSSEEnvelopes(t, ctx, resp, 2)
	var sess *sseFrame
	for i := range frames {
		if frames[i].Event == "session-"+expectedID {
			sess = &frames[i]
			break
		}
	}
	if sess == nil {
		t.Fatalf("missing session frame for session_end:\n%+v", frames)
	}
	if !strings.Contains(sess.Data, "status-ended") {
		t.Errorf("session_end should drive status-ended; got:\n%s", sess.Data)
	}
}

func TestStream_RefusesOverCap(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	// Saturate up to the cap with long-lived connections, then
	// confirm the next attempt gets 429.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < streamMaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", base+"/stream", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			// Hold the connection by reading until ctx cancels.
			buf := make([]byte, 4096)
			for {
				if _, err := resp.Body.Read(buf); err != nil {
					return
				}
			}
		}()
	}

	// Brief settle — the goroutines need to actually issue
	// their requests so streamCount reflects them.
	time.Sleep(200 * time.Millisecond)

	overReq, _ := http.NewRequestWithContext(t.Context(), "GET", base+"/stream", nil)
	resp, err := http.DefaultClient.Do(overReq)
	if err != nil {
		t.Fatalf("GET (over-cap): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("over-cap status: got %d, want 429", resp.StatusCode)
	}

	cancel()
	wg.Wait()
}
