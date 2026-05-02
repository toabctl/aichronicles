package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
)

// RegisterAichroniclesAPITools registers the MCP tools that have
// migrated off direct *store.Store access and now read through
// internal/apiclient. Tools live here as their handlers move; the
// remainder are still registered by RegisterAichroniclesTools and
// RegisterAichroniclesAnalyticsTools (in tools_aichronicles.go and
// tools_analytics.go).
//
// Production wiring calls all three so the tool catalog is the
// union of registered tools regardless of which side a handler
// happens to be on. A tool MUST appear in exactly one registry —
// duplicate registration would have the second Register call
// overwrite the first silently.
func RegisterAichroniclesAPITools(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "get_unresolved_for_cwd",
		Description: "Pull every unresolved item the past summaries left behind in the given cwd. " +
			"Each item is one outstanding TODO captured in a summary's $.unresolved array. " +
			"Use as a SessionStart context-injection tool: the agent reads what was left dangling " +
			"the last few times the user worked here. " +
			"Returns one line per item with session-short id, relative time, summary topic, and the item itself. " +
			"Empty when nothing is dangling — same shape as the `aichronicles unresolved` CLI.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cwd":             {"type": "string",  "description": "exact cwd match (required)"},
				"since_days":      {"type": "integer", "minimum": 1, "default": 30, "description": "only consider sessions ended within this many days"},
				"max_sessions":    {"type": "integer", "minimum": 1, "default": 5,  "description": "cap on prior sessions feeding the output"},
				"max_per_session": {"type": "integer", "minimum": 1, "default": 5,  "description": "cap on items per source session"}
			},
			"required": ["cwd"]
		}`),
		Handler: getUnresolvedForCwdAPIHandler(c),
	})
}

// getUnresolvedForCwdAPIHandler is the apiclient-backed
// replacement for getUnresolvedForCwdHandler. Same shape on the
// wire as the legacy version; transport is HTTP+JSON over UDS.
func getUnresolvedForCwdAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Cwd           string `json:"cwd"`
			SinceDays     int    `json:"since_days"`
			MaxSessions   int    `json:"max_sessions"`
			MaxPerSession int    `json:"max_per_session"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "get_unresolved_for_cwd: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Cwd) == "" {
			return TextError("get_unresolved_for_cwd: cwd is required"), nil
		}
		days := req.SinceDays
		if days <= 0 {
			days = 30
		}
		maxSessions := req.MaxSessions
		if maxSessions <= 0 {
			maxSessions = 5
		}
		maxPerSession := req.MaxPerSession
		if maxPerSession <= 0 {
			maxPerSession = 5
		}
		sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

		resp, err := c.Unresolved(ctx, apiclient.UnresolvedRequest{
			Cwd:                req.Cwd,
			SinceMs:            sinceMs,
			MaxSessions:        maxSessions,
			MaxItemsPerSession: maxPerSession,
		})
		if err != nil {
			// Daemon-unreachable is the dominant failure mode here;
			// surface it cleanly so the agent knows to retry rather
			// than think the user has nothing dangling.
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_unresolved_for_cwd: load: " + err.Error()}
		}
		if len(resp.Items) == 0 {
			return TextResult(fmt.Sprintf("no unresolved items from prior sessions in %s", req.Cwd)), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d unresolved item(s) from prior sessions in %s:\n", len(resp.Items), req.Cwd)
		now := time.Now()
		for _, it := range resp.Items {
			when := relativeAgo(it.EndedAtMs, now)
			topic := it.Topic
			if topic == "" {
				topic = "(no summary topic)"
			}
			fmt.Fprintf(&b, "  • [%s, %s] %s — %s\n",
				it.SessionShort, when, topic, it.Item)
		}
		return TextResult(b.String()), nil
	}
}
