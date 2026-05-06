package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

// envelopeNDJSON marshals env as one NDJSON line (JSON + \n).
func envelopeNDJSON(t *testing.T, env events.Envelope) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

func TestHandleImport_EmptyBody(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(nil)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ImportStats
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.LinesRead != 0 || out.Imported != 0 {
		t.Errorf("empty body should produce zero counts: %+v", out)
	}
}

func TestHandleImport_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	var body bytes.Buffer
	for i := 0; i < 5; i++ {
		body.Write(envelopeNDJSON(t, validEnvelope(t)))
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", &body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ImportStats
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.LinesRead != 5 {
		t.Errorf("LinesRead: got %d, want 5", out.LinesRead)
	}
	if out.Imported != 5 {
		t.Errorf("Imported: got %d, want 5", out.Imported)
	}
	if out.Invalid != 0 || out.Deduped != 0 {
		t.Errorf("expected no invalid/deduped: %+v", out)
	}
}

func TestHandleImport_DuplicatesAreDeduped(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	env := validEnvelope(t)
	line := envelopeNDJSON(t, env)

	body := bytes.Buffer{}
	body.Write(line)
	body.Write(line) // exact duplicate
	body.Write(line) // and another

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", &body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ImportStats
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Imported != 1 || out.Deduped != 2 {
		t.Errorf("expected 1 imported + 2 deduped; got %+v", out)
	}
}

func TestHandleImport_InvalidLinesAreSkipped(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	good1 := envelopeNDJSON(t, validEnvelope(t))
	good2 := envelopeNDJSON(t, validEnvelope(t))
	body := bytes.Buffer{}
	body.Write(good1)
	body.WriteString("not json\n")             // malformed JSON
	body.WriteString(`{"event_id":""}` + "\n") // valid JSON, fails Validate
	body.Write(good2)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", &body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ImportStats
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Imported != 2 {
		t.Errorf("Imported: got %d, want 2", out.Imported)
	}
	if out.Invalid != 2 {
		t.Errorf("Invalid: got %d, want 2", out.Invalid)
	}
}

func TestHandleImport_BlankLinesIgnored(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	good := envelopeNDJSON(t, validEnvelope(t))
	body := bytes.Buffer{}
	body.WriteString("\n\n  \n")
	body.Write(good)
	body.WriteString("\n\n")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", &body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ImportStats
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Imported != 1 || out.Invalid != 0 {
		t.Errorf("blank lines should not count: %+v", out)
	}
}

func TestHandleImport_RejectsUnsupportedContentType(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/import", strings.NewReader("ignored"))
	req.Header.Set("Content-Type", "application/xml")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status=%d, want 415", rr.Code)
	}
}

func TestHandleImport_AcceptsCommonContentTypes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	good := envelopeNDJSON(t, validEnvelope(t))
	for _, ct := range []string{"application/x-ndjson", "application/json", "text/plain", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(good))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("Content-Type=%q: status=%d, want 200", ct, rr.Code)
		}
	}
}

func TestHandleImport_MultipleSessionsLandSeparately(t *testing.T) {
	t.Parallel()
	// Two distinct sessions in one stream — server-side derived
	// session_id should distinguish them. Sanity that the
	// streaming path doesn't share state across lines.
	srv := newTestServer(t)
	envA := validEnvelope(t)
	envA.SourceSessionID = "sess-stream-A"
	envA.EventID = uuid.Must(uuid.NewV7()).String()
	envB := validEnvelope(t)
	envB.SourceSessionID = "sess-stream-B"
	envB.EventID = uuid.Must(uuid.NewV7()).String()

	body := bytes.Buffer{}
	body.Write(envelopeNDJSON(t, envA))
	body.Write(envelopeNDJSON(t, envB))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", &body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var sessions int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 2 {
		t.Errorf("expected 2 sessions; got %d", sessions)
	}
}

func TestHandleImport_RedactsSecretsServerSide(t *testing.T) {
	t.Parallel()
	// Streaming path must apply server-side redaction same as
	// /v1/ingest does: a leaked secret in a transcript line
	// must not be persisted verbatim.
	srv := newTestServer(t)
	env := validEnvelope(t)
	env.ContentText = "leak: AKIAIOSFODNN7EXAMPLE end"
	env.Redaction = nil
	body := envelopeNDJSON(t, env)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var content string
	if err := srv.store.DB().QueryRow(
		`SELECT content_text FROM events WHERE event_id = ?`, env.EventID).Scan(&content); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if strings.Contains(content, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("secret survived in stored content: %q", content)
	}
	if !strings.Contains(content, "<redacted:aws_access_key>") {
		t.Errorf("expected redacted marker; got %q", content)
	}
}
