package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

func TestHandleSearch_RequiresQ(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSearch_EmptyDB_ReturnsEmptyHits(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=hello", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.SearchResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Hits) != 0 {
		t.Errorf("got %d hits, want 0", len(out.Hits))
	}
	if !contains(rr.Body.String(), `"hits":[]`) {
		t.Errorf("expected hits:[]; got %s", rr.Body.String())
	}
}

func TestHandleSearch_FindsIngestedContent(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.ContentText = "the quick brown fox"
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env)))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=brown", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.SearchResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Hits) != 1 {
		t.Errorf("got %d hits, want 1", len(out.Hits))
	}
}

func TestHandleSearch_RejectsBadParams(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, p := range []string{
		"/v1/search?q=hi&since_ms=-1",
		"/v1/search?q=hi&limit=0",
		"/v1/search?q=hi&limit=abc",
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status=%d, want 400", p, rr.Code)
		}
	}
}
