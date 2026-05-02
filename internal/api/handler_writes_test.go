package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toabctl/aichronicles/pkg/api"
	"github.com/toabctl/aichronicles/pkg/events"
)

func hashOf(t *testing.T, s string) string {
	t.Helper()
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestHandleLLMOutputSave_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	req := api.SaveLLMOutputRequest{
		Kind:        "summary",
		Model:       "claude-sonnet-4-6",
		PromptHash:  hashOf(t, "hello"),
		Body:        "summary body",
		CreatedAtMs: 1,
	}
	body := mustJSON(t, req)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/llm-outputs", bytesReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out api.SaveLLMOutputResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.ID <= 0 || !out.Inserted {
		t.Errorf("expected ID>0, Inserted=true; got %+v", out)
	}

	// Idempotency: same hash → existing id, Inserted=false.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/llm-outputs", bytesReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("second status=%d", rr.Code)
	}
	var out2 api.SaveLLMOutputResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out2)
	if out2.ID != out.ID {
		t.Errorf("idempotency: id changed from %d to %d", out.ID, out2.ID)
	}
	if out2.Inserted {
		t.Errorf("second call must report Inserted=false; got %+v", out2)
	}
}

func TestHandleLLMOutputSave_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, body := range []string{
		`{"model":"m"}`, // no kind
		`{"kind":"summary","model":"m","body":"x"}`,        // no prompt_hash
		`{"kind":"summary","model":"m","prompt_hash":"h"}`, // no body
		`{"kind":"summary","extra":"unknown"}`,             // unknown field
		`not json`,
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/v1/llm-outputs", bytesReader([]byte(body))))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %q: status=%d, want 400", body, rr.Code)
		}
	}
}

func TestHandleEpisodesSave_RejectsMissingSessionID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/episodes", bytesReader([]byte(`{}`))))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandleSessionLinksSave_RejectsBadKind(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := mustJSON(t, api.SaveSessionLinksRequest{
		FromSessionID: "from-1",
		Links:         []api.SessionLink{{ToSessionID: "to-1", Kind: "invented_kind"}},
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/session-links", bytesReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for invalid link kind", rr.Code)
	}
}

func TestHandleSessionOutcomeSave_RejectsBadOutcome(t *testing.T) {
	t.Parallel()
	// The store's CHECK constraint allows only the four canonical
	// labels. An invalid outcome must surface as a 4xx, not as a
	// silent 500. Today the constraint failure does map to 500
	// (the store doesn't translate it) — this test pins the
	// current behavior so a future fix can flip the assertion.
	srv := newTestServer(t)
	body := mustJSON(t, api.SaveSessionOutcomeRequest{
		SessionID: "no-such-session",
		Outcome:   "neutral",
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/session-outcomes", bytesReader(body)))
	if rr.Code == http.StatusOK || rr.Code == http.StatusNoContent {
		t.Errorf("expected error for invalid outcome, got %d", rr.Code)
	}
}

func TestHandleSessionOutcomeSave_ValidOutcomeWritesRow(t *testing.T) {
	t.Parallel()
	// Ingest first so the session row exists (FK + outcomes
	// upsert path). Use a real outcome label from the store
	// constraint.
	srv := newTestServer(t)
	env := validEnvelope(t)
	srv.Handler().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(mustJSON(t, env))))

	sessID := events.DeriveSessionID("claude-code", "sess-abc")
	body := mustJSON(t, api.SaveSessionOutcomeRequest{
		SessionID:    sessID,
		ComputedAtMs: 1,
		Outcome:      "unknown",
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/session-outcomes", bytesReader(body)))
	if rr.Code != http.StatusNoContent {
		t.Errorf("status=%d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}
