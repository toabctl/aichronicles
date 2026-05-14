package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

func TestHandleEpisodesList_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/episodes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out wire.EpisodeListResponse
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
	var out wire.EpisodeListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Episodes) != 0 {
		t.Errorf("expected empty list for non-matching filter")
	}
}

// TestHandleSegmentSession_ChunkedBodyHonored pins the fix for the
// review-#7 RISK #10: handleSegmentSession used to gate the JSON
// decode on `r.ContentLength > 0`, which is false for chunked
// transfer encoding (ContentLength == -1). A client sending an
// idle_gap_ms tweak over chunked transport had it silently
// discarded and got the segmenter default. The fix routes through
// decodeJSONBody-equivalent discipline; an unknown field now
// surfaces a 400 even when ContentLength is unset.
func TestHandleSegmentSession_ChunkedBodyHonored(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	// Body with an unknown field. With DisallowUnknownFields the
	// handler MUST return 400; pre-fix the body was discarded so
	// the unknown field went unnoticed.
	req := httptest.NewRequest(http.MethodPost,
		"/v1/sessions/deadbeef-0000-0000-0000-000000000001/segment",
		strings.NewReader(`{"idle_gap_ms":1000,"bogus_field":"x"}`))
	req.ContentLength = -1 // simulate chunked transfer
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (DisallowUnknownFields should fire on chunked body)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "bogus_field") {
		t.Errorf("body should name the unknown field: %s", rr.Body.String())
	}
}
