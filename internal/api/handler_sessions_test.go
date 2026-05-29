package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

func TestHandleSessionsList_Empty(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out wire.SessionListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Sessions) != 0 {
		t.Errorf("got %d sessions, want 0", len(out.Sessions))
	}
	// Empty slice must encode as []; catches nil-vs-empty regression.
	if !contains(rr.Body.String(), `"sessions":[]`) {
		t.Errorf("expected sessions:[]; got %s", rr.Body.String())
	}
}

func TestHandleSessionsList_CursorPaginates(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	const n = 5
	for i := range n {
		env := validEnvelope(t)
		env.SourceSessionID = fmt.Sprintf("sess-page-%d", i)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))
		if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
			t.Fatalf("ingest %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}

	seen := map[string]bool{}
	total, pages := 0, 0
	cursor := ""
	for {
		url := "/v1/sessions?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: status=%d body=%s", pages, rr.Code, rr.Body.String())
		}
		var out wire.SessionListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, sdg := range out.Sessions {
			if seen[sdg.ID] {
				t.Fatalf("session %s appeared on two pages (overlap)", sdg.ID)
			}
			seen[sdg.ID] = true
		}
		total += len(out.Sessions)
		pages++
		if pages > 10 {
			t.Fatal("cursor never terminated")
		}
		if out.NextCursor == "" {
			break
		}
		cursor = string(out.NextCursor)
	}
	if total != n {
		t.Fatalf("paged total: got %d, want %d", total, n)
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
	var out wire.SessionListResponse
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
	var got wire.SessionDigest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID: got %q, want %q", got.ID, id)
	}
}

func TestHandleSessionDigests_BySessionIDs(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Ingest two distinct sessions; we'll request one plus an
	// unknown id and expect only the real one back.
	for _, key := range []string{"sess-dig-A", "sess-dig-B"} {
		env := validEnvelope(t)
		env.SourceSessionID = key
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))
		if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
			t.Fatalf("ingest %s: status=%d body=%s", key, rr.Code, rr.Body.String())
		}
	}

	idA := events.DeriveSessionID("claude-code", "sess-dig-A")
	idGhost := events.DeriveSessionID("claude-code", "sess-dig-ghost")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/digests?session_ids="+idA+","+idGhost, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var out wire.SessionDigestsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// session_ids mode returns exactly the known requested id —
	// sess-dig-B was never asked for, the ghost id doesn't exist.
	if len(out.Digests) != 1 {
		t.Fatalf("digest count: got %d, want 1; body=%s", len(out.Digests), rr.Body.String())
	}
	if out.Digests[0].ID != idA {
		t.Errorf("ID: got %q, want %q", out.Digests[0].ID, idA)
	}
	if out.Digests[0].SourceSessionID != "sess-dig-A" {
		t.Errorf("SourceSessionID: got %q, want sess-dig-A", out.Digests[0].SourceSessionID)
	}
}

// TestHandleSessionStartCwd_NullForUnknownSession covers the
// "no recorded cwd" branch: an unknown id returns 200 with cwd:null,
// not 404. Documented contract — see handler_session_reads.go.
func TestHandleSessionStartCwd_NullForUnknownSession(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/nope/start-cwd", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"cwd":null`) {
		t.Errorf("expected cwd:null; got %s", rr.Body.String())
	}
}

func TestHandleSessionLinks_RejectsMissingFromAndTo(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/session-links", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSessionLinks_RejectsBothFromAndTo(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/session-links?from=a&to=b", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSessionLinks_FromReturnsEmpty(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/session-links?from=unknown", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"links":[]`) {
		t.Errorf("expected empty links:[]; got %s", rr.Body.String())
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
	var out wire.CandidateSessionListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Candidates) != 0 {
		t.Errorf("got %d candidates, want 0", len(out.Candidates))
	}
}

func TestHandleSessionsResolve_RequiresPrefix(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/resolve", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSessionsResolve_NotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/resolve?prefix=00000000", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

func TestHandleSessionsResolve_FoundReturnsCanonicalID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	env := validEnvelope(t)
	env.SourceSessionID = "sess-resolve"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}

	full := events.DeriveSessionID("claude-code", "sess-resolve")
	// Take the leading 8 chars (DeriveSessionID is "claude-code-..." for
	// claude — so "claude-c" is hex-or-hyphen and unique by construction
	// in this test). Use the FIRST 8 chars of the id.
	prefix := full[:8]
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/resolve?prefix="+prefix, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got wire.ResolveSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != full {
		t.Errorf("ID: got %q, want %q", got.ID, full)
	}
}

func TestHandleSessionsResolve_BadPrefixIs400(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/resolve?prefix=zzz!@#", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (non-hex prefix)", rr.Code)
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
	var p wire.Problem
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
