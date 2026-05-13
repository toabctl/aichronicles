package mcp

import (
	"net/http/httptest"
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
