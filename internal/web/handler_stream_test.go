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
)

// readSSEFrames reads SSE-formatted bytes from r until ctx is done
// or n frames arrive. Returns the parsed event payloads. Used by
// the stream tests to inspect what reached the wire without baking
// in tight timing assumptions.
func readSSEFrames(t *testing.T, ctx context.Context, r *http.Response, n int) []map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	var (
		frames  []map[string]any
		curData strings.Builder
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
			curData.WriteString(strings.TrimPrefix(line, "data:"))
		case line == "":
			// Frame terminator.
			if curData.Len() > 0 {
				var m map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(curData.String())), &m); err == nil {
					frames = append(frames, m)
					if len(frames) >= n {
						return frames
					}
				}
				curData.Reset()
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
	got := frames[0]
	if got["kind"] != "user_prompt" {
		t.Errorf("kind: got %v, want user_prompt", got["kind"])
	}
	snippet, _ := got["snippet"].(string)
	if !strings.Contains(snippet, "live event content") {
		t.Errorf("snippet: got %q, want substring 'live event content'", snippet)
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
	if frames[0]["session_id"] != wantedID {
		t.Errorf("filter leaked: got session_id %v, want %s", frames[0]["session_id"], wantedID)
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
