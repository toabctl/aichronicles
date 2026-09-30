package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

// ingestSearchDoc ingests one user_prompt with the given session id
// and content so search-pagination tests can seed a controlled corpus.
func ingestSearchDoc(t *testing.T, srv *testServer, sessionID, content string) {
	t.Helper()
	env := validEnvelope(t)
	env.SourceSessionID = sessionID
	env.ContentText = content
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))
	if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
		t.Fatalf("ingest %s: %d %s", sessionID, rr.Code, rr.Body.String())
	}
}

// searchPage runs one GET /v1/search page and decodes the response.
func searchPage(t *testing.T, srv *testServer, urlStr string) wire.SearchResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, urlStr, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", urlStr, rr.Code, rr.Body.String())
	}
	var out wire.SearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

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

func TestHandleSearch_CursorPaginatesWithoutOverlap(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	const n = 5
	for i := range n {
		ingestSearchDoc(t, srv, fmt.Sprintf("sess-pager-%d", i), fmt.Sprintf("pagerToken doc %d", i))
	}

	seen := map[string]bool{}
	total, pages := 0, 0
	cursor := ""
	for {
		url := "/v1/search?q=pagerToken&limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor // base64url is URL-safe, no escaping needed
		}
		out := searchPage(t, srv, url)
		for _, h := range out.Hits {
			if seen[h.SessionID] {
				t.Fatalf("session %s appeared on two pages (overlap)", h.SessionID)
			}
			seen[h.SessionID] = true
		}
		total += len(out.Hits)
		pages++
		if pages > 10 {
			t.Fatal("cursor never terminated (no empty NextCursor)")
		}
		if out.NextCursor == "" {
			break
		}
		cursor = string(out.NextCursor)
	}
	if total != n {
		t.Fatalf("paged total: got %d rows, want %d", total, n)
	}
}

func TestHandleSearch_NextCursorEmptyOnShortPage(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestSearchDoc(t, srv, "sess-short-0", "shortToken a")
	ingestSearchDoc(t, srv, "sess-short-1", "shortToken b")

	out := searchPage(t, srv, "/v1/search?q=shortToken&limit=5")
	if len(out.Hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(out.Hits))
	}
	if out.NextCursor != "" {
		t.Errorf("short page must have empty NextCursor, got %q", out.NextCursor)
	}
}

func TestHandleSearch_FullPageEmitsCursorThenDrains(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	const n = 3
	for i := range n {
		ingestSearchDoc(t, srv, fmt.Sprintf("sess-exact-%d", i), fmt.Sprintf("exactToken %d", i))
	}

	// limit == result count → a full page that emits a cursor.
	first := searchPage(t, srv, "/v1/search?q=exactToken&limit=3")
	if len(first.Hits) != 3 {
		t.Fatalf("first page: got %d hits, want 3", len(first.Hits))
	}
	if first.NextCursor == "" {
		t.Fatal("a full page must emit a NextCursor")
	}
	// Following it drains to zero rows and an empty cursor — correct,
	// not a bug.
	second := searchPage(t, srv, "/v1/search?q=exactToken&limit=3&cursor="+string(first.NextCursor))
	if len(second.Hits) != 0 {
		t.Errorf("drained page: got %d hits, want 0", len(second.Hits))
	}
	if second.NextCursor != "" {
		t.Errorf("drained page must have empty NextCursor, got %q", second.NextCursor)
	}
}

func TestHandleSearch_MalformedCursorIs400(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/search?q=hi&cursor=%21%21%21notbase64", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("malformed cursor: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleSearch_OffsetTooDeepIs400(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	deep, err := wire.EncodeSearchCursor(wire.SearchCursor{
		Off: wire.MaxOffset, Stage: "primary", Now: 1,
	})
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/search?q=hi&cursor="+string(deep), nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("deep offset: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
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

// TestHandleSearch_FindsNonASCIIWords proves the tokenizer fix end to
// end: a bare non-ASCII word used to reach FTS5 as a split, invalid-
// UTF-8 fragment and matched nothing.
func TestHandleSearch_FindsNonASCIIWords(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestSearchDoc(t, srv, "sess-fr", "voilà ça marche enfin")
	ingestSearchDoc(t, srv, "sess-zh", "你好 世界")
	for _, q := range []string{"voilà", "你好"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q="+url.QueryEscape(q), nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("q=%q: status=%d body=%s", q, rr.Code, rr.Body.String())
		}
		var got wire.SearchResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Hits) != 1 {
			t.Errorf("q=%q: got %d hits, want 1", q, len(got.Hits))
		}
	}
}

// TestHandleSearch_RejectsForgedCursorFields pins cursor validation:
// a well-formed cursor carrying an unknown stage used to 500 in the
// store, and a negative offset slipped past the depth guard.
func TestHandleSearch_RejectsForgedCursorFields(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for name, c := range map[string]wire.SearchCursor{
		"unknown stage":   {Off: 20, Stage: "bogus"},
		"empty stage":     {Off: 20, Stage: ""},
		"negative offset": {Off: -5, Stage: "primary"},
		"unknown order":   {Off: 20, Stage: "primary", Ord: 7},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			enc, err := wire.EncodeSearchCursor(c)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=fox&cursor="+string(enc), nil))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestHandleSearch_CursorIsBoundToItsQuery is the regression gate for
// cursors re-sent with a different q or filters: the old offset and
// locked FTS stage were silently applied to the new query, skipping
// rows or mixing corpora. The server must refuse the mismatch and
// keep accepting the cursor with its own query.
func TestHandleSearch_CursorIsBoundToItsQuery(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for i := range 3 {
		ingestSearchDoc(t, srv, fmt.Sprintf("sess-bind-%d", i), fmt.Sprintf("bindToken doc %d", i))
	}
	first := searchPage(t, srv, "/v1/search?q=bindToken&limit=1")
	if first.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	cur := "&cursor=" + string(first.NextCursor)
	for name, path := range map[string]string{
		"different q":      "/v1/search?q=otherToken&limit=1" + cur,
		"added filter":     "/v1/search?q=bindToken&kind=user_prompt&limit=1" + cur,
		"changed since_ms": "/v1/search?q=bindToken&since_ms=5&limit=1" + cur,
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", name, rr.Code)
		}
	}
	// Same query, different page size: still the same result set.
	if got := searchPage(t, srv, "/v1/search?q=bindToken&limit=2"+cur); len(got.Hits) != 2 {
		t.Errorf("same query: got %d hits, want the remaining 2", len(got.Hits))
	}
}

// TestHandleSearch_NULIs400 pins the NUL-byte query to a client error;
// it reached SQLite and came back 500 "unterminated string".
func TestHandleSearch_NULIs400(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=foo%00bar", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
}
