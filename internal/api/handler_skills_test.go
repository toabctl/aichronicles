package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleSkillsStaleness_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/skills/staleness", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"skills":`) {
		t.Errorf("expected skills key; got %s", rr.Body.String())
	}
}

func TestHandleSkillsStaleness_RejectsBadParams(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, p := range []string{
		"/v1/skills/staleness?since_ms=-1",
		"/v1/skills/staleness?window_ms=-1",
		"/v1/skills/staleness?max_skills=0",
		"/v1/skills/staleness?max_examples=abc",
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status=%d, want 400", p, rr.Code)
		}
	}
}
