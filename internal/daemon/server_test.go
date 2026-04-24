package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/ingest"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	lg, err := OpenLogger(logPath)
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return NewServer(lg, nil), logPath
}

func validBody(t *testing.T) []byte {
	t.Helper()
	e := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-abc",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "hi"},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestIngest_Accepts_ValidEnvelope(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(validBody(t)))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
	}
	var ack ingest.Ack
	if err := json.Unmarshal(rr.Body.Bytes(), &ack); err != nil {
		t.Fatalf("ack decode: %v", err)
	}
	if ack.EventID == "" || ack.SessionID == "" {
		t.Fatalf("expected populated ack, got %+v", ack)
	}
	if ack.Deduped {
		t.Fatalf("expected Deduped=false for first write")
	}
}

func TestIngest_Rejects_MalformedJSON(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader("{not json"))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected problem+json, got %q", ct)
	}
}

func TestIngest_Rejects_InvalidEnvelope(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	// v=1 is required; omit it to trigger validation
	body := []byte(`{"event_id":"00000000-0000-0000-0000-000000000000","source_agent":"x","source_session_id":"s","kind":"k","ts_source":"2026-01-01T00:00:00Z","payload":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIngest_Rejects_UnknownEnvelopeFields(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	body := bytes.Join([][]byte{validBody(t)[:len(validBody(t))-1], []byte(`,"extra":"nope"}`)}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}
