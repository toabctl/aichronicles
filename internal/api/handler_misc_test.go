package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// One representative happy-path / validation test per misc
// endpoint. Deeper coverage of the underlying store calls lives
// in internal/store/*_test.go and is not duplicated here.

func TestHandleSummaries_RequiresSessionID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/summaries", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSummaries_NotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/summaries?session_id=ghost", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

func TestHandleSummariesBatch_RequiresIDs(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/summaries/batch", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSummariesBatch_UnknownIDsReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/summaries/batch?session_ids=ghost1,ghost2", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"summaries":{}`) {
		t.Errorf("expected empty summaries:{}; got %s", rr.Body.String())
	}
}

func TestHandleLLMOutput_RequiresKindAndHash(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, p := range []string{
		"/v1/llm-outputs",
		"/v1/llm-outputs?kind=summary",
		"/v1/llm-outputs?prompt_hash=abc",
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status=%d, want 400", p, rr.Code)
		}
	}
}

func TestHandleUnresolved_RequiresCwd(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/unresolved", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleProjectsAggregates_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/projects/aggregates", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"projects":[]`) {
		t.Errorf("expected projects:[]; got %s", rr.Body.String())
	}
}

func TestHandleSubagents_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/subagents", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"spans":[]`) {
		t.Errorf("expected spans:[]; got %s", rr.Body.String())
	}
}

func TestHandleInsights_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/insights", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// insights wraps a JSON object with window/overview/etc.; on
	// an empty DB the window still renders.
	if !contains(rr.Body.String(), `"window"`) {
		t.Errorf("expected window key; got %s", rr.Body.String())
	}
}

func TestHandleMisc_RejectsBadParams(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, p := range []string{
		"/v1/unresolved?cwd=/p&since_ms=-1",
		"/v1/unresolved?cwd=/p&max_sessions=-3",
		"/v1/unresolved?cwd=/p&max_items_per_session=abc",
		"/v1/projects/aggregates?since_ms=-1",
		"/v1/insights?since_ms=-1",
		"/v1/insights?top_tools=0",
		"/v1/insights?top_skills=abc",
		"/v1/subagents?limit=0",
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status=%d, want 400", p, rr.Code)
		}
	}
}
