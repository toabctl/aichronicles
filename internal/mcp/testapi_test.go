package mcp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/store"
)

// newAPITestClient stands up an httptest.Server holding the real
// internal/api handlers backed by st, and returns an
// apiclient.Client pointed at it. Used by tests of MCP tools that
// have migrated to RegisterAichroniclesAPITools.
//
// Cleanup is automatic via t.Cleanup; callers do not need to
// close anything manually.
func newAPITestClient(t *testing.T, st *store.Store) *apiclient.Client {
	t.Helper()
	srv := httptest.NewServer(api.NewServer(st, nil).Handler())
	t.Cleanup(srv.Close)
	return apiclient.NewClientForTesting(srv.Client(), srv.URL)
}

// registerAllTools is the test-time companion to the production
// wiring in cli/mcp_serve.go: register every aichronicles tool
// regardless of which side (store-backed or apiclient-backed) the
// handler currently lives on. Tests that need any tool present
// in s.tools call this so they don't have to track which
// registrar to invoke for which tool.
func registerAllTools(t *testing.T, s *Server, st *store.Store) {
	t.Helper()
	c := newAPITestClient(t, st)
	RegisterAichroniclesAnalyticsTools(s, c)
	RegisterAichroniclesAPITools(s, c)
}

// capturedQueries records the query of every api request a test's MCP
// tools make, keyed by path, so a test can assert what a tool actually
// sent (e.g. that a schema bound was applied client-side).
type capturedQueries struct {
	mu   sync.Mutex
	byPC map[string][]url.Values
}

func (c *capturedQueries) get(path string) []url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byPC[path]
}

// registerAllToolsCapturing is registerAllTools with a recording
// middleware in front of the real api handlers.
func registerAllToolsCapturing(t *testing.T, s *Server, st *store.Store) *capturedQueries {
	t.Helper()
	cq := &capturedQueries{byPC: map[string][]url.Values{}}
	inner := api.NewServer(st, nil).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cq.mu.Lock()
		cq.byPC[r.URL.Path] = append(cq.byPC[r.URL.Path], r.URL.Query())
		cq.mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := apiclient.NewClientForTesting(srv.Client(), srv.URL)
	RegisterAichroniclesAnalyticsTools(s, c)
	RegisterAichroniclesAPITools(s, c)
	return cq
}
