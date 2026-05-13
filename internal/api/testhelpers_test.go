package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// testServer wraps a production *Server and shadows its Handler()
// so a successful POST /v1/ingest drives Worker().drain()
// inline before returning to the caller. This restores the
// synchronous semantics the pre-async pipeline gave tests
// (POST → assert downstream state) without forcing every test
// to add a manual drain call.
//
// Embedding (rather than holding the *Server as a named field)
// promotes every field and method of *Server transparently, so
// existing tests that read srv.store, srv.sseBus, etc. keep
// compiling unchanged.
type testServer struct {
	*Server
}

// Handler shadows *Server.Handler. It wraps the production
// handler with a tiny middleware: on a 2xx response to POST
// /v1/ingest, run a single drain pass so the test's next
// assertion sees the redacted/extracted row in events /
// raw_envelopes / extractions. Other routes pass through
// unmodified.
func (ts *testServer) Handler() http.Handler {
	inner := ts.Server.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest" {
			return
		}
		// Only drain when the ingest succeeded — a 4xx/5xx
		// means nothing landed in ingest_pending for us to
		// process, and draining anyway risks the test seeing
		// stale unrelated rows.
		if rr, ok := w.(*httptest.ResponseRecorder); ok && rr.Code/100 == 2 {
			_ = ts.Server.Worker().drain(r.Context())
		}
	})
}

// Test-only helpers shared across handler_*_test.go files. Keep
// them tiny — each helper exists because the same boilerplate
// shows up in 3+ tests.

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
