package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

func TestHandleScrub_DryRunOnEmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := mustJSON(t, wire.ScrubRequest{DryRun: new(true)})
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

// TestAdminDestructive_RequireExplicitDryRun is the regression gate
// for the scrub/prune default: `dry_run` was a plain bool whose zero
// value is false, so an empty scrub body — which the wire docs called
// "dry-run by default" — ran an irreversible live rewrite, and a prune
// body without dry_run deleted for real. Both endpoints must refuse
// to infer a mode, and must not touch the store when they refuse.
func TestAdminDestructive_RequireExplicitDryRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		body string
	}{
		{"scrub empty body", "/v1/scrub", ""},
		{"scrub empty object", "/v1/scrub", `{}`},
		{"scrub null dry_run", "/v1/scrub", `{"dry_run":null}`},
		{"scrub trailing data", "/v1/scrub", `{"dry_run":true}{"x":1}`},
		{"scrub unknown field", "/v1/scrub", `{"dry_run":true,"dryrun":false}`},
		{"prune empty body", "/v1/prune", ""},
		{"prune missing dry_run", "/v1/prune", `{"cutoff_ms":1000}`},
		{"prune null dry_run", "/v1/prune", `{"cutoff_ms":1000,"dry_run":null}`},
		{"prune trailing data", "/v1/prune", `{"cutoff_ms":1000,"dry_run":true}{"x":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t)
			env := validEnvelope(t)
			env.ContentText = "leak: AKIAIOSFODNN7EXAMPLE end"
			seedRawSecret(t, srv, env)

			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
			}
			var content string
			if err := srv.store.DB().QueryRow(
				`SELECT content_text FROM events WHERE event_id = ?`, env.EventID,
			).Scan(&content); err != nil {
				t.Fatalf("refused request must leave the row in place: %v", err)
			}
			if !strings.Contains(content, "AKIAIOSFODNN7EXAMPLE") {
				t.Errorf("refused request still rewrote the row: %q", content)
			}
		})
	}
}

// TestHandleScrub_ExplicitModes pins both explicit modes against a
// store holding a raw secret: dry_run=true reports without writing,
// dry_run=false rewrites.
func TestHandleScrub_ExplicitModes(t *testing.T) {
	t.Parallel()
	for _, dry := range []bool{true, false} {
		srv := newTestServer(t)
		env := validEnvelope(t)
		env.ContentText = "leak: AKIAIOSFODNN7EXAMPLE end"
		seedRawSecret(t, srv, env)

		rr := httptest.NewRecorder()
		body := mustJSON(t, wire.ScrubRequest{DryRun: new(dry)})
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/scrub", bytesReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("dry_run=%v: status=%d body=%s", dry, rr.Code, rr.Body.String())
		}
		var out wire.ScrubResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.DryRun != dry {
			t.Errorf("dry_run=%v: response reports dry_run=%v", dry, out.DryRun)
		}
		var content string
		if err := srv.store.DB().QueryRow(
			`SELECT content_text FROM events WHERE event_id = ?`, env.EventID,
		).Scan(&content); err != nil {
			t.Fatal(err)
		}
		if leaked := strings.Contains(content, "AKIAIOSFODNN7EXAMPLE"); leaked != dry {
			t.Errorf("dry_run=%v: secret still present=%v, want %v", dry, leaked, dry)
		}
	}
}

// seedRawSecret stores env with its secret intact by calling
// store.IngestEnvelope directly — the "stored before the detector
// existed" state scrub exists to repair.
func seedRawSecret(t *testing.T, srv *testServer, env events.Envelope) {
	t.Helper()
	raw := mustJSON(t, env)
	tx, err := srv.store.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
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

	scrubBody := mustJSON(t, wire.ScrubRequest{DryRun: new(false)})
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

// TestHandlePrune_RejectsCutoffOutsideNow pins both "prune every
// ended session" extremes: a non-positive cutoff and one in the
// future. A cutoff just below now (what the CLI sends) is accepted.
func TestHandlePrune_RejectsCutoffOutsideNow(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixMilli()
	cases := []struct {
		name   string
		cutoff int64
		want   int
	}{
		{"zero", 0, http.StatusBadRequest},
		{"negative", -1, http.StatusBadRequest},
		{"one hour ahead", now + int64(time.Hour/time.Millisecond), http.StatusBadRequest},
		{"far future", 99_999_999_999_999, http.StatusBadRequest},
		{"just before now", now - 1, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t)
			body := mustJSON(t, wire.PruneRequest{CutoffMs: tc.cutoff, DryRun: new(true)})
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/prune", bytesReader(body)))
			if rr.Code != tc.want {
				t.Errorf("cutoff_ms=%d: status=%d body=%s, want %d", tc.cutoff, rr.Code, rr.Body.String(), tc.want)
			}
		})
	}
}

func TestHandlePrune_DryRunOnEmptyDB(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := mustJSON(t, wire.PruneRequest{CutoffMs: 1000, DryRun: new(true)})
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
