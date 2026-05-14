package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readSSEEventsUntil consumes lines from the SSE response until
// it has seen `want` data lines (i.e., complete event frames) or
// the deadline fires. Returns the raw concatenated body so the
// caller can assert on the JSON payloads.
//
// SSE frames look like:
//
//	id: 1
//	event: event
//	data: {...}
//	<blank>
//
// Counting "data:" lines is the correct seam for "complete frame
// arrived" — counting "event:" can stop mid-frame before the
// payload arrives.
func readSSEEventsUntil(t *testing.T, body io.Reader, want int, deadline time.Duration) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		sc := bufio.NewScanner(body)
		seen := 0
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte('\n')
			if strings.HasPrefix(sc.Text(), "data:") {
				seen++
				if seen >= want {
					done <- b.String()
					return
				}
			}
		}
		done <- b.String()
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(deadline):
		t.Fatalf("timed out waiting for %d SSE event blocks", want)
		return ""
	}
}

func TestHandleStream_DeliversIngestedEventToLiveSubscriber(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	defer srv.Close()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// Subscribe in the background.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/v1/stream", nil)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: got %q", got)
	}

	// Publish via the ingest path so the bus fires for real.
	go func() {
		// small delay so the subscriber loop is parked on the bus channel
		time.Sleep(50 * time.Millisecond)
		env := validEnvelope(t)
		body := mustJSON(t, env)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(body)))
		if rr.Code != http.StatusOK {
			t.Errorf("ingest status=%d body=%s", rr.Code, rr.Body.String())
		}
	}()

	// Read the SSE stream until we see the event frame.
	got := readSSEEventsUntil(t, resp.Body, 1, 3*time.Second)
	if !strings.Contains(got, `"event_id"`) {
		t.Errorf("expected event_id in stream payload; got %q", got)
	}
	if !strings.Contains(got, "event: event") {
		t.Errorf("expected 'event: event' frame; got %q", got)
	}
}

// TestHandleStream_ReplaysSinceSeq pins the SSE resume contract:
// a client that reconnects with Last-Event-ID (or ?since_seq=)
// receives every event with a higher ingest_seq from durable
// storage before the bridge to the live bus picks up. Without
// this, every event written during a reconnect gap is lost.
func TestHandleStream_ReplaysSinceSeq(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	defer srv.Close()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// Pre-seed three events into the store via the ingest path —
	// each lands an ingest_seq 1, 2, 3 with deterministic content.
	for i := 0; i < 3; i++ {
		env := validEnvelope(t)
		body := mustJSON(t, env)
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(body))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("ingest %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}

	// Subscribe with Last-Event-ID=1; expect to replay seqs 2 and 3.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/v1/stream", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got := readSSEEventsUntil(t, resp.Body, 2, 3*time.Second)
	if !strings.Contains(got, "id: 2") {
		t.Errorf("expected replay frame id=2; got %q", got)
	}
	if !strings.Contains(got, "id: 3") {
		t.Errorf("expected replay frame id=3; got %q", got)
	}
	if strings.Contains(got, "id: 1") {
		t.Errorf("seq=1 should not replay (already delivered): %q", got)
	}
}

// TestHandleStream_SinceSeqQueryParam pins the non-SSE entry
// point: the `since_seq` query param resumes the same way the
// Last-Event-ID header does, so curl-shaped clients and tests can
// opt into replay without setting headers. When both are
// supplied, the query param wins.
func TestHandleStream_SinceSeqQueryParam(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	defer srv.Close()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	for i := 0; i < 2; i++ {
		env := validEnvelope(t)
		body := mustJSON(t, env)
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(body))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("ingest %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/v1/stream?since_seq=1", nil)
	// Header set to a value the query param should override.
	req.Header.Set("Last-Event-ID", "999")
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got := readSSEEventsUntil(t, resp.Body, 1, 3*time.Second)
	if !strings.Contains(got, "id: 2") {
		t.Errorf("expected replay frame id=2 via since_seq query param; got %q", got)
	}
}

func TestHandleStream_429WhenAtCapacity(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	defer srv.Close()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// Saturate the bus by subscribing through the http path.
	// Using the bus directly would not exercise the handler's
	// 429 mapping, which is the property under test.
	cancels := make([]context.CancelFunc, 0, SSEMaxSubscribers)
	bodies := make([]io.ReadCloser, 0, SSEMaxSubscribers)
	defer func() {
		for _, b := range bodies {
			_ = b.Close()
		}
		for _, c := range cancels {
			c()
		}
	}()
	for i := 0; i < SSEMaxSubscribers; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/v1/stream", nil)
		resp, err := httpSrv.Client().Do(req)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("subscribe %d: status=%d", i, resp.StatusCode)
		}
		bodies = append(bodies, resp.Body)
	}

	// One more — must 429.
	resp, err := httpSrv.Client().Get(httpSrv.URL + "/v1/stream")
	if err != nil {
		t.Fatalf("over-cap subscribe transport: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429; got %d", resp.StatusCode)
	}
}
