package mcp

import (
	"errors"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

// mapAPIError is the canonical apiclient-error → MCP-result mapping
// every tool runs after a c.X(...) call. Surfaces ErrSocketUnavailable
// as a tool-level error so the agent learns "the daemon's down" (not
// "the request failed for an unspecified reason"); falls back to a
// protocol-level Error with a "<tool>: " prefix.
//
// Returns (nil, nil) on a nil error so callers can write
// `if r, e := mapAPIError(...); r != nil || e != nil { return r, e }`.
func mapAPIError(toolName string, err error) (*ToolResult, *Error) {
	if err == nil {
		return nil, nil
	}
	if errors.Is(err, apiclient.ErrSocketUnavailable) {
		return TextError("aichronicles-api unreachable; is the daemon running?"), nil
	}
	return nil, &Error{Code: InternalError, Message: toolName + ": " + err.Error()}
}

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

// first8 returns the first 8 chars of a session id; thin alias over
// preview.ShortID for callers in this package that want the short
// id by its conventional MCP name.
func first8(s string) string { return preview.ShortID(s) }

// formatTS renders an epoch-millis as the canonical RFC3339 form
// MCP responses use. Wraps internal/timefmt so MCP and the web
// stay aligned.
func formatTS(ms int64) string {
	return timefmt.AbsoluteRFC3339(ms)
}
