package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/pkg/api"
)

func TestHandleFactsSubjects_RequiresContains(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/facts/subjects", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleFactsSubjects_EmptyResultIsArray(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/facts/subjects?contains=zzz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"subjects":[]`) {
		t.Errorf("expected subjects:[]; got %s", rr.Body.String())
	}
}

func TestHandleFactsList_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/facts", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out api.FactsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Facts) != 0 {
		t.Errorf("got %d facts, want 0", len(out.Facts))
	}
}

func TestHandleFacts_RejectsBadLimit(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, p := range []string{"/v1/facts?limit=0", "/v1/facts?limit=abc", "/v1/facts/subjects?limit=-3"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status=%d, want 400", p, rr.Code)
		}
	}
}
