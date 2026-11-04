package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
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

// TestLLMOutputReads_CarryCacheTokens is the regression gate for the
// dropped prompt-cache counters: they were saved and scanned but
// llmOutputToWire never copied them, so every read route returned a
// row as if its cache usage had never been captured. A row written
// without them must still read back with the fields omitted.
func TestLLMOutputReads_CarryCacheTokens(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	save := func(hash string, write, read *int64) int64 {
		t.Helper()
		body := mustJSON(t, wire.SaveLLMOutputRequest{
			Kind: "reflect", Model: "m", PromptHash: hash,
			InputTokens: new(int64(5)), OutputTokens: new(int64(6)),
			CacheWriteTokens: write, CacheReadTokens: read,
			Body: "{}", CreatedAtMs: 1_760_000_000_000,
		})
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/llm-outputs", bytesReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("save: status=%d body=%s", rr.Code, rr.Body.String())
		}
		var out wire.SaveLLMOutputResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.ID
	}
	withCache := save("h-cache", new(int64(11)), new(int64(22)))
	noCache := save("h-old", nil, nil)

	get := func(path string, into any) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
	check := func(route string, o wire.LLMOutput) {
		t.Helper()
		switch o.ID {
		case withCache:
			if o.CacheWriteTokens == nil || *o.CacheWriteTokens != 11 ||
				o.CacheReadTokens == nil || *o.CacheReadTokens != 22 {
				t.Errorf("%s: cache tokens not carried: write=%v read=%v", route, o.CacheWriteTokens, o.CacheReadTokens)
			}
		case noCache:
			if o.CacheWriteTokens != nil || o.CacheReadTokens != nil {
				t.Errorf("%s: uncaptured cache tokens must stay nil: write=%v read=%v", route, o.CacheWriteTokens, o.CacheReadTokens)
			}
		}
	}

	var byID wire.LLMOutput
	get("/v1/llm-outputs/"+itoa(withCache), &byID)
	check("by id", byID)
	var byHash wire.LLMOutput
	get("/v1/llm-outputs/by-hash?kind=reflect&prompt_hash=h-cache", &byHash)
	check("by hash", byHash)
	var list wire.LLMOutputsListResponse
	get("/v1/llm-outputs?kind=reflect", &list)
	if len(list.Outputs) != 2 {
		t.Fatalf("list: got %d rows, want 2", len(list.Outputs))
	}
	for _, o := range list.Outputs {
		check("list", o)
	}
}

// TestHandleSessionLLMOutputs_FiltersInSQLAndPages is the regression
// gate for the per-session list's silent cut: an unfiltered request
// stopped at limit with no next_cursor, so rows past the first page
// were unreachable. The kind filter must also still find an old row
// behind a page of newer other-kind ones.
func TestHandleSessionLLMOutputs_FiltersInSQLAndPages(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	env := validEnvelope(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: %d", rr.Code)
	}
	id := events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	save := func(kind, hash string, at int64) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/llm-outputs", bytesReader(mustJSON(t, wire.SaveLLMOutputRequest{
			SessionID: &id, Kind: kind, Model: "m", PromptHash: hash, Body: "{}", CreatedAtMs: at,
		}))))
		if rr.Code != http.StatusOK {
			t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
		}
	}
	save("summary", "the-summary", 1_000)
	for i := range wire.DefaultPageLimit + 5 {
		save("facts", "f"+strconv.Itoa(i), int64(2_000+i)) // all newer than the summary
	}

	get := func(q string) wire.LLMOutputsListResponse {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+id+"/llm-outputs?"+q, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", q, rr.Code, rr.Body.String())
		}
		var out wire.LLMOutputsListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := get("kind=summary"); len(got.Outputs) != 1 || got.Outputs[0].PromptHash != "the-summary" {
		t.Errorf("kind=summary: got %d rows, want the one summary", len(got.Outputs))
	}
	// Unfiltered: a full first page with a cursor, and the cursor
	// reaches every remaining row.
	first := get("")
	if len(first.Outputs) != wire.DefaultPageLimit || first.NextCursor == "" {
		t.Fatalf("first page: %d rows, cursor %q", len(first.Outputs), first.NextCursor)
	}
	rest := get("cursor=" + string(first.NextCursor))
	if total := len(first.Outputs) + len(rest.Outputs); total != wire.DefaultPageLimit+6 {
		t.Errorf("paged %d rows in total, want %d", total, wire.DefaultPageLimit+6)
	}
}

// TestHandleSummariesGet_PicksNewestSummary pins /v1/summaries after
// its move to the SQL-filtered query.
func TestHandleSummariesGet_PicksNewestSummary(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	id := "sess-summary-pick"
	if _, err := srv.store.DB().Exec(`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', 'x')`, id); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"summary", "summary", "facts"} {
		if _, err := srv.store.DB().Exec(`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms) VALUES (?, ?, ?, ?, 'm', ?)`,
			id, kind, kind+strconv.Itoa(i), "h"+strconv.Itoa(i), 100+i); err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/summaries?session_id="+id, nil))
	var got wire.LLMOutput
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("status %d: %v %s", rr.Code, err, rr.Body.String())
	}
	if got.Body != "summary1" {
		t.Errorf("got body %q, want the newest summary", got.Body)
	}
}
