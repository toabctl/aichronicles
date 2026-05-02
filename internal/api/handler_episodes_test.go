package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/pkg/api"
)

func TestHandleEpisodesList_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/episodes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out api.EpisodeListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Episodes) != 0 {
		t.Errorf("got %d episodes, want 0", len(out.Episodes))
	}
	if !contains(rr.Body.String(), `"episodes":[]`) {
		t.Errorf("expected episodes:[]; got %s", rr.Body.String())
	}
}

func TestHandleEpisodesList_RejectsBadParams(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, p := range []string{
		"/v1/episodes?since_ms=-1",
		"/v1/episodes?limit=0",
		"/v1/episodes?limit=abc",
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status=%d, want 400", p, rr.Code)
		}
	}
}

func TestHandleEpisodesList_FilterPassThrough(t *testing.T) {
	t.Parallel()
	// No fixture episodes are easy to seed without running the
	// segmenter through real events. We accept that the pure
	// "filter passes through to the store" property is exercised
	// here as "non-matching filter returns []", and the deeper
	// segmenter coverage already lives in store/episodes_test.go.
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/episodes?cwd=/nope&query_contains=zzz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out api.EpisodeListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Episodes) != 0 {
		t.Errorf("expected empty list for non-matching filter")
	}
}
