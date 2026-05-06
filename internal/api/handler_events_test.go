package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/pkg/api"
)

// ingestN posts n distinct envelopes through the server and returns
// the event_ids in ingest order. Used to seed events for the list-
// endpoint tests.
func ingestN(t *testing.T, srv *Server, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		env := validEnvelope(t)
		ids = append(ids, env.EventID)
		body := mustJSON(t, env)
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(body))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("seed ingest %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	return ids
}

func TestHandleEventsList_EmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out api.EventListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Events) != 0 {
		t.Errorf("Events: got %d, want 0", len(out.Events))
	}
	if out.LatestSeq != 0 {
		t.Errorf("LatestSeq: got %d, want 0", out.LatestSeq)
	}
	// Empty events slice MUST encode as []; catches a regression
	// where we return nil and break clients that range over the
	// field without nil-checking.
	if !contains(rr.Body.String(), `"events":[]`) {
		t.Errorf("expected events:[]; got %s", rr.Body.String())
	}
}

func TestHandleEventsList_ReturnsAllInOrder(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestN(t, srv, 5)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out api.EventListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Events) != 5 {
		t.Fatalf("Events: got %d, want 5", len(out.Events))
	}
	if out.LatestSeq < 5 {
		t.Errorf("LatestSeq: got %d, want >=5", out.LatestSeq)
	}
	// Strict monotonicity: ingest_seq is the canonical ordering.
	for i := 1; i < len(out.Events); i++ {
		if out.Events[i].IngestSeq <= out.Events[i-1].IngestSeq {
			t.Errorf("not sorted: events[%d].IngestSeq=%d <= events[%d].IngestSeq=%d",
				i, out.Events[i].IngestSeq, i-1, out.Events[i-1].IngestSeq)
		}
	}
}

func TestHandleEventsList_FiltersBySessionID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Session A: 2 events.
	for i := 0; i < 2; i++ {
		env := validEnvelope(t)
		env.SourceSessionID = "sess-A"
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env)))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
	}
	// Session B: 3 events.
	for i := 0; i < 3; i++ {
		env := validEnvelope(t)
		env.SourceSessionID = "sess-B"
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env)))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
	}

	// Filter to A: should see only 2.
	wantSessA := events.DeriveSessionID("claude-code", "sess-A")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events?session_id="+wantSessA, nil))
	var out api.EventListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Events) != 2 {
		t.Errorf("filter A: got %d events, want 2", len(out.Events))
	}
	for _, e := range out.Events {
		if e.SessionID != wantSessA {
			t.Errorf("expected session %q, got %q", wantSessA, e.SessionID)
		}
	}
}

func TestHandleEventsList_SinceSeqWatermark(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestN(t, srv, 3)

	// First fetch: no cursor, expect 3.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	var page1 api.EventListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &page1)
	if len(page1.Events) != 3 {
		t.Fatalf("page1 len: got %d, want 3", len(page1.Events))
	}

	// Second fetch with since_seq = highest from page1: empty.
	highSeq := page1.Events[len(page1.Events)-1].IngestSeq
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet,
			"/v1/events?since_seq="+itoa(highSeq), nil))
	var page2 api.EventListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &page2)
	if len(page2.Events) != 0 {
		t.Errorf("page2 len: got %d, want 0 (caught up)", len(page2.Events))
	}
	if page2.LatestSeq != highSeq {
		t.Errorf("LatestSeq watermark: got %d, want %d", page2.LatestSeq, highSeq)
	}
}

func TestHandleEventsList_LimitClampsToMax(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestN(t, srv, 3)

	// Ask for more than MaxPageLimit; server clamps. We don't have
	// MaxPageLimit-many rows seeded, so just verify the response
	// is still well-formed and len <= MaxPageLimit.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet,
			"/v1/events?limit="+itoa(int64(api.MaxPageLimit*2)), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out api.EventListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Events) > api.MaxPageLimit {
		t.Errorf("got %d events; expected <= MaxPageLimit (%d)",
			len(out.Events), api.MaxPageLimit)
	}
}

func TestHandleEventsList_RejectsNegativeSinceSeq(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events?since_seq=-1", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleEventsList_RejectsZeroLimit(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events?limit=0", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleEventsList_RejectsNonNumericLimit(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events?limit=abc", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleEventsList_NonExistentSessionReturnsEmpty(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestN(t, srv, 2)

	// Filter by a session that doesn't exist - empty list, NOT 404.
	// Filtering is "give me the events matching this", and an
	// empty match is a valid answer.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events?session_id=nonexistent", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", rr.Code)
	}
	var out api.EventListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Events) != 0 {
		t.Errorf("got %d events; want 0", len(out.Events))
	}
}

func TestHandleEventsList_NullableFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// One envelope WITH cwd, one WITHOUT.
	withCwd := validEnvelope(t)
	withCwd.Cwd = "/tmp/with"
	withCwd.EventID = uuid.Must(uuid.NewV7()).String()
	withoutCwd := validEnvelope(t)
	withoutCwd.Cwd = ""
	withoutCwd.EventID = uuid.Must(uuid.NewV7()).String()

	for _, env := range []events.Envelope{withCwd, withoutCwd} {
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env)))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("seed: status=%d body=%s", rr.Code, rr.Body.String())
		}
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	var out api.EventListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(out.Events))
	}

	var sawCwd, sawNoCwd bool
	for _, e := range out.Events {
		if e.EventID == withCwd.EventID {
			if e.Cwd == nil || *e.Cwd != "/tmp/with" {
				t.Errorf("withCwd row Cwd: got %v, want \"/tmp/with\"", e.Cwd)
			}
			sawCwd = true
		}
		if e.EventID == withoutCwd.EventID {
			if e.Cwd != nil {
				t.Errorf("withoutCwd row Cwd: got %v, want nil", *e.Cwd)
			}
			sawNoCwd = true
		}
	}
	if !sawCwd || !sawNoCwd {
		t.Errorf("did not see both rows in response")
	}
}

func TestHandleEventsList_ContextCancellationStopsQuery(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ingestN(t, srv, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	// Pre-canceled context is reported by the SQL driver as an
	// error → 500. We don't enforce a specific status, just that
	// it's not a 200 with bogus data.
	if rr.Code == http.StatusOK {
		var out api.EventListResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		if len(out.Events) > 0 {
			t.Errorf("canceled context returned %d events", len(out.Events))
		}
	}
}
