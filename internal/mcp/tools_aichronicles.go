package mcp

import (
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

// RegisterAichroniclesTools is preserved for callers that have not
// migrated to the apiclient registrar set. After Phase B, every
// tool that used to live here has moved to RegisterAichroniclesAPITools
// (in tools_apiclient.go). This function is now a no-op kept only
// so existing wiring code (cli/mcp_serve.go, integration tests)
// keeps compiling — it can be removed once the call sites are
// audited and dropped.
func RegisterAichroniclesTools(_ *Server, _ *store.Store) {}

// relativeAgo formats epoch-millis as a short relative time. Wraps
// internal/timefmt so MCP, web, and CLI agree on the thresholds and
// labels; the only MCP-specific override is the empty-state token —
// "active" instead of "-" so the agent reading the tool result
// sees a verbal cue that the session is mid-flight.
func relativeAgo(ms int64, now time.Time) string {
	if ms <= 0 {
		return "active"
	}
	return timefmt.Relative(ms, now)
}

// first8 returns the first 8 chars of a session id, or the full id
// when shorter. Used as the conventional short-id form across MCP
// tool responses.
func first8(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// formatTS renders an epoch-millis as the canonical RFC3339 form
// MCP responses use. Wraps internal/timefmt so MCP and the web
// stay aligned.
func formatTS(ms int64) string {
	return timefmt.AbsoluteRFC3339(ms)
}
