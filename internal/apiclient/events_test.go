package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/toabctl/aichronicles/pkg/api"
)

func TestClient_Events_HappyPath(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)

	// Seed: 3 ingests so the server has something to list.
	for i := 0; i < 3; i++ {
		if _, err := c.Ingest(context.Background(), validEnvelope(t)); err != nil {
			t.Fatalf("seed Ingest %d: %v", i, err)
		}
	}

	out, err := c.Events(context.Background(), api.EventListRequest{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(out.Events) != 3 {
		t.Errorf("Events: got %d, want 3", len(out.Events))
	}
	if out.LatestSeq < 3 {
		t.Errorf("LatestSeq: got %d, want >=3", out.LatestSeq)
	}
}

func TestClient_Events_FiltersOmitZero(t *testing.T) {
	t.Parallel()
	// Verify the request encoding never sends spurious empty
	// query params. A server that DisallowsUnknownFields on its
	// query strings would (in a future world) reject those, so
	// we make sure the client's URL builder is tight.
	var seenQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"latest_seq":0}`))
	}))
	t.Cleanup(srv.Close)
	c := newClientForTests(srv.Client(), srv.URL)

	if _, err := c.Events(context.Background(), api.EventListRequest{}); err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, k := range []string{"session_id", "since_seq", "limit"} {
		if seenQuery.Has(k) {
			t.Errorf("server received unwanted query param %q on zero-valued request: %v", k, seenQuery)
		}
	}
}

func TestClient_Events_FiltersForwarded(t *testing.T) {
	t.Parallel()
	var seenQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"latest_seq":0}`))
	}))
	t.Cleanup(srv.Close)
	c := newClientForTests(srv.Client(), srv.URL)

	req := api.EventListRequest{SessionID: "sess-x", SinceSeq: 42, Limit: 10}
	if _, err := c.Events(context.Background(), req); err != nil {
		t.Fatalf("Events: %v", err)
	}
	if got := seenQuery.Get("session_id"); got != "sess-x" {
		t.Errorf("session_id: got %q, want sess-x", got)
	}
	if got := seenQuery.Get("since_seq"); got != "42" {
		t.Errorf("since_seq: got %q, want 42", got)
	}
	if got := seenQuery.Get("limit"); got != "10" {
		t.Errorf("limit: got %q, want 10", got)
	}
}

func TestClient_Events_400Error(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"title":"Bad","status":400,"detail":"limit must be positive"}`))
	}))
	t.Cleanup(srv.Close)
	c := newClientForTests(srv.Client(), srv.URL)

	_, err := c.Events(context.Background(), api.EventListRequest{Limit: -1})
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) || herr.Status != 400 {
		t.Errorf("expected HTTPError 400, got %T %v", err, err)
	}
}
