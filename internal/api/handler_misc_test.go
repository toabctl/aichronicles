package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
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
		"/v1/llm-outputs/by-hash",
		"/v1/llm-outputs/by-hash?kind=summary",
		"/v1/llm-outputs/by-hash?prompt_hash=abc",
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

// TestHandleLLMOutputsList_WireShape pins the JSON envelope of the
// list endpoints to wire.LLMOutputsListResponse so the server and
// apiclient can never silently disagree on the "outputs" key.
func TestHandleLLMOutputsList_WireShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/llm-outputs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got wire.LLMOutputsListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got.Outputs == nil {
		t.Errorf("Outputs is nil, want empty slice")
	}
}

func TestHandleLLMOutputsLastCreated_WireShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/llm-outputs/last-created-at?kind=summary", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got wire.LLMOutputLastCreatedAtResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"last_created_at_ms"`) {
		t.Errorf("missing last_created_at_ms key: %s", rr.Body.String())
	}
}

func TestHandleLLMOutputExists_WireShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/llm-outputs/exists?session_id=ghost&kind=summary", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got wire.LLMOutputExistsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got.Exists {
		t.Errorf("Exists=true for ghost session, want false")
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
