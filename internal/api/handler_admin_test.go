package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

func TestHandleScrub_DryRunOnEmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := mustJSON(t, wire.ScrubRequest{DryRun: true})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/scrub", bytesReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ScrubResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.DryRun {
		t.Errorf("DryRun should round-trip true: %+v", out)
	}
	if out.EventsScanned != 0 {
		t.Errorf("empty DB should scan 0 events, got %d", out.EventsScanned)
	}
}

func TestHandleScrub_EmptyBodyDefaultsToDryRun(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/scrub", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleScrub_NoOpAfterServerSideRedaction(t *testing.T) {
	t.Parallel()
	// The api redacts on the ingest path, so a fresh DB seeded
	// only via /v1/ingest is already scrubbed — a follow-up
	// scrub run must report zero rewrites. Acts as the
	// "scrubber idempotent under server-side redaction"
	// regression test: a future change that re-introduces edge
	// redaction without server-side redaction would silently
	// store a secret and surface as a non-zero rewrite count
	// here. Deeper rewrite-correctness tests live in
	// internal/store/scrub_test.go (preexisting).
	srv := newTestServer(t)

	env := validEnvelope(t)
	env.ContentText = "leak: AKIAIOSFODNN7EXAMPLE end"
	env.Redaction = nil
	body := mustJSON(t, env)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/ingest", bytesReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("seed ingest: status=%d body=%s", rr.Code, rr.Body.String())
	}

	scrubBody := mustJSON(t, wire.ScrubRequest{DryRun: false})
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/scrub", bytesReader(scrubBody)))
	if rr.Code != http.StatusOK {
		t.Fatalf("scrub: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.ScrubResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.EnvelopesRewritten != 0 {
		t.Errorf("server-side-redacted ingest should leave nothing for scrub to rewrite; got %+v", out)
	}

	var raw string
	if err := srv.store.DB().QueryRow(
		`SELECT envelope_json FROM raw_envelopes WHERE event_id = ?`, env.EventID,
	).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if strings.Contains(raw, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("ingest-time redaction missed the secret: %s", raw)
	}
}

func TestHandlePrune_RequiresBody(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/prune", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

func TestHandlePrune_RejectsZeroCutoff(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := mustJSON(t, wire.PruneRequest{CutoffMs: 0})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/prune", bytesReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (cutoff_ms=0 must be refused)", rr.Code)
	}
}

func TestHandlePrune_DryRunOnEmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := mustJSON(t, wire.PruneRequest{CutoffMs: 1000, DryRun: true})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/prune", bytesReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.PruneResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Sessions != 0 {
		t.Errorf("empty DB should report 0 sessions; got %d", out.Sessions)
	}
	if !out.DryRun {
		t.Errorf("DryRun must round-trip; got %+v", out)
	}
}

func TestHandleIngestStats_EmptyQueueReturnsZeros(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out wire.IngestStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Pending != 0 {
		t.Errorf("empty queue Pending: got %d, want 0", out.Pending)
	}
	if out.OldestAgeMs != 0 {
		t.Errorf("empty queue OldestAgeMs: got %d, want 0", out.OldestAgeMs)
	}
	if out.MaxAttempts != 0 {
		t.Errorf("empty queue MaxAttempts: got %d, want 0", out.MaxAttempts)
	}
	if out.Capacity != DefaultIngestQueueMax {
		t.Errorf("Capacity: got %d, want %d", out.Capacity, DefaultIngestQueueMax)
	}
}

func TestHandleIngestStats_ReflectsBacklog(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Use the underlying store layer to stage a row directly,
	// bypassing the handler's auto-drain test wrapper.
	body := []byte(`{"v":1,"event_id":"x","source_agent":"a","source_session_id":"s",` +
		`"kind":"k","ts_source":"2026-05-13T10:00:00Z","payload":{},"redaction":{"applied":true}}`)
	if _, err := srv.store.DB().Exec(
		`INSERT INTO ingest_pending(event_id, body, received_at_ms, attempt_count)
		 VALUES (?, ?, ?, ?)`,
		"evt-stats-1", body, 1000, 3,
	); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out wire.IngestStatsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Pending != 1 {
		t.Errorf("Pending: got %d, want 1", out.Pending)
	}
	if out.MaxAttempts != 3 {
		t.Errorf("MaxAttempts: got %d, want 3", out.MaxAttempts)
	}
	if out.OldestAgeMs <= 0 {
		t.Errorf("OldestAgeMs: got %d, want >0 (now − received_at_ms=1000)", out.OldestAgeMs)
	}
}
