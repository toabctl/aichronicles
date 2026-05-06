package apiclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/wire"
)

// newTestClient stands up an httptest.Server with the supplied
// handler and returns a Client pointed at it. tearDown is invoked
// on cleanup. Used by every error-mapping test below.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return newClientForTests(srv.Client(), srv.URL)
}

func TestDo_GET_Decodes200Body(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	var out struct {
		Hello string `json:"hello"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/anything", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.Hello != "world" {
		t.Errorf("decode: got %q", out.Hello)
	}
}

func TestDo_POST_MarshalsJSONBody(t *testing.T) {
	t.Parallel()
	var seen string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		seen = string(buf[:n])
		w.WriteHeader(http.StatusNoContent)
	}))
	body := map[string]string{"x": "y"}
	if err := c.do(context.Background(), http.MethodPost, "/x", body, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if seen != `{"x":"y"}` {
		t.Errorf("server saw %q; want canonical JSON", seen)
	}
}

func TestDo_204_NoBody_NoError(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	var out struct{ X string }
	if err := c.do(context.Background(), http.MethodGet, "/x", nil, &out); err != nil {
		t.Errorf("204 produced error: %v", err)
	}
	if out.X != "" {
		t.Errorf("204 must not populate target")
	}
}

func TestDo_400_DecodesProblem_AndIsBadRequestError(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"title":"Bad","status":400,"detail":"missing foo"}`)
	}))
	err := c.do(context.Background(), http.MethodPost, "/x", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if herr.Status != 400 {
		t.Errorf("Status: got %d", herr.Status)
	}
	if herr.Problem.Title != "Bad" || herr.Problem.Detail != "missing foo" {
		t.Errorf("Problem decoded incorrectly: %+v", herr.Problem)
	}
}

func TestDo_404_MapsToErrNotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	err := c.do(context.Background(), http.MethodGet, "/missing", nil, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDo_413_MapsToErrTooLarge(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	err := c.do(context.Background(), http.MethodPost, "/x", nil, nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestDo_409_MapsToErrConflict(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	err := c.do(context.Background(), http.MethodPost, "/x", nil, nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("got %v, want ErrConflict", err)
	}
}

func TestDo_500_MapsToErrServer(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	err := c.do(context.Background(), http.MethodGet, "/x", nil, nil)
	if !errors.Is(err, ErrServer) {
		t.Errorf("got %v, want ErrServer", err)
	}
}

func TestDo_4xxNoBody_StillProducesHTTPError(t *testing.T) {
	t.Parallel()
	// Defensive: a server that returns 405 Method Not Allowed
	// without a problem+json body must still produce a usable
	// HTTPError carrying the status code so the caller can
	// distinguish from transport failures.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	err := c.do(context.Background(), http.MethodPost, "/x", nil, nil)
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if herr.Status != http.StatusMethodNotAllowed {
		t.Errorf("Status: got %d", herr.Status)
	}
}

func TestDo_MalformedProblemJSON_StillTyped(t *testing.T) {
	t.Parallel()
	// Some upstream proxy or buggy server might return 400 with
	// a non-JSON body. Don't let json decode panic; surface
	// the status with empty Problem fields.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not json"))
	}))
	err := c.do(context.Background(), http.MethodGet, "/x", nil, nil)
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if herr.Status != 400 {
		t.Errorf("Status: got %d", herr.Status)
	}
}

func TestDo_UnexpectedContentType_OnSuccess_IsError(t *testing.T) {
	t.Parallel()
	// 200 with text/plain when a JSON body was expected: surface
	// as a typed error so callers don't mis-decode silently.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	}))
	var out struct{}
	err := c.do(context.Background(), http.MethodGet, "/x", nil, &out)
	if err == nil {
		t.Fatal("expected content-type error")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Errorf("expected content-type message, got %v", err)
	}
}

func TestDo_MalformedJSONOnSuccess_IsWrappedError(t *testing.T) {
	t.Parallel()
	// 200 with declared application/json but a truncated body:
	// json.Decoder returns io.ErrUnexpectedEOF; we wrap it.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"x":`)) // truncated
	}))
	var out struct{ X int }
	err := c.do(context.Background(), http.MethodGet, "/x", nil, &out)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestDo_ContextCancellation_StopsRequest(t *testing.T) {
	t.Parallel()
	// Server hangs forever; cancelling the context must surface
	// as a context error, not a timeout, and the request must
	// not leave goroutines behind.
	started := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	var doErr error
	go func() {
		defer wg.Done()
		doErr = c.do(ctx, http.MethodGet, "/x", nil, nil)
	}()

	// Wait for the server to be holding the request, then cancel.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received request")
	}
	cancel()
	wg.Wait()

	if doErr == nil {
		t.Fatal("expected error from canceled context")
	}
	if !errors.Is(doErr, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", doErr)
	}
}

func TestDo_SocketDoesNotExist_IsErrSocketUnavailable(t *testing.T) {
	t.Parallel()
	// Production-shape constructor: point at a nonexistent UDS
	// path; the dialer fails with ENOENT and we surface that as
	// ErrSocketUnavailable with the path in the message.
	missing := filepath.Join(t.TempDir(), "no-such.sock")
	c := NewClient(missing)
	err := c.do(context.Background(), http.MethodGet, "/healthz", nil, nil)
	if !errors.Is(err, ErrSocketUnavailable) {
		t.Errorf("got %v, want ErrSocketUnavailable", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("expected path in error message, got %v", err)
	}
}

func TestDo_Concurrent_RequestsDoNotInterfere(t *testing.T) {
	t.Parallel()
	// Sanity: many goroutines through the same Client must each
	// receive their own response. Catches a regression where a
	// shared buffer or shared decoder leaks state.
	var hit atomic.Int64
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"n":%d}`, n)
	}))

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make(chan int, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			var out struct {
				N int `json:"n"`
			}
			if err := c.do(context.Background(), http.MethodGet, "/x", nil, &out); err != nil {
				t.Errorf("do: %v", err)
				return
			}
			results <- out.N
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[int]bool)
	for n := range results {
		if seen[n] {
			t.Errorf("duplicate response n=%d (cross-talk)", n)
		}
		seen[n] = true
	}
	if len(seen) != goroutines {
		t.Errorf("got %d unique responses; want %d", len(seen), goroutines)
	}
}

func TestHTTPError_FormatsTitleAndDetail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  HTTPError
		want string
	}{
		{
			name: "title and detail",
			err:  HTTPError{Status: 400, Problem: wire.Problem{Title: "Bad", Detail: "missing foo"}},
			want: "apiclient: 400 Bad: missing foo",
		},
		{
			name: "title only",
			err:  HTTPError{Status: 404, Problem: wire.Problem{Title: "Not Found"}},
			want: "apiclient: 404 Not Found",
		},
		{
			name: "neither",
			err:  HTTPError{Status: 500},
			want: "apiclient: HTTP 500",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
