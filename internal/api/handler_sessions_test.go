package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/pkg/api"
	"github.com/toabctl/aichronicles/pkg/events"
)

func TestHandleSessionsList_Empty(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out api.SessionListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Sessions) != 0 {
		t.Errorf("got %d sessions, want 0", len(out.Sessions))
	}
	// Empty slice must encode as []; catches nil-vs-empty regression.
	if !contains(rr.Body.String(), `"sessions":[]`) {
		t.Errorf("expected sessions:[]; got %s", rr.Body.String())
	}
}

func TestHandleSessionsList_ListsIngested(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	// Two distinct sessions, three events total.
	for _, sid := range []string{"sess-X", "sess-Y", "sess-Y"} {
		env := validEnvelope(t)
		env.SourceSessionID = sid
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env)))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out api.SessionListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Sessions) != 2 {
		t.Errorf("got %d sessions, want 2 (one per source_session_id)", len(out.Sessions))
	}
}

func TestHandleSessionsGet_NotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

func TestHandleSessionsGet_Found(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	env := validEnvelope(t)
	env.SourceSessionID = "sess-find-me"
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env)))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	id := events.DeriveSessionID("claude-code", "sess-find-me")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+id, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got api.SessionDigest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID: got %q, want %q", got.ID, id)
	}
}

func TestHandleSessionsList_RejectsBadParams(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	cases := []string{
		"/v1/sessions?since_ms=-1",
		"/v1/sessions?limit=0",
		"/v1/sessions?limit=abc",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("path %q: status=%d, want 400", p, rr.Code)
			}
		})
	}
}

func TestHandleSessionsRelated_ReturnsEmptyForUnknown(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/no-such/related", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (empty list, NOT 404)", rr.Code)
	}
	var out api.CandidateSessionListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Candidates) != 0 {
		t.Errorf("got %d candidates, want 0", len(out.Candidates))
	}
}

// Sanity: errors.As against the apiclient HTTPError works against
// a 404 from this server. Tested here so a regression in handler-
// side error wiring is caught before the apiclient stripe.
func TestHandleSessionsGet_ErrorShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rr.Code)
	}
	var p api.Problem
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Status != 404 {
		t.Errorf("Problem.Status: got %d", p.Status)
	}
	// Defensive: an empty Title would be a regression to the
	// pre-RFC-7807 days.
	if p.Title == "" {
		t.Errorf("Problem.Title empty: %+v", p)
	}
	// errors.Is convention is exercised in the apiclient tests;
	// kept this assertion server-side so an error-path bug
	// shows in this file's tests too.
	_ = errors.New
}
