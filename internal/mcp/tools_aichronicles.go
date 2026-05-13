package mcp

import (
	"time"

	"github.com/toabctl/aichronicles/internal/timefmt"
)

// This file holds the formatting helpers shared across the MCP
// tool registrars. The legacy RegisterAichroniclesTools entry
// point (a *store.Store-receiving no-op kept transiently while
// the apiclient migration landed) was removed in this commit; all
// MCP tools now read through internal/apiclient against the
// aichronicles-api daemon, matching the "out-of-process readers
// go through the wire" policy codified in tools/depcheck.

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
