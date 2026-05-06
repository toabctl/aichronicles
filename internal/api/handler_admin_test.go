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
