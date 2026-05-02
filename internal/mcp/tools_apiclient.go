package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/pkg/api"
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
	registerGetUnresolvedForCwd(s, c)
	registerGetFactsForSubject(s, c)
	registerFindFactSubjects(s, c)
	registerGetSkillStaleness(s, c)
	registerGetInsights(s, c)
}

func registerGetUnresolvedForCwd(s *Server, c *apiclient.Client) {
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

// --- get_facts_for_subject ---

func registerGetFactsForSubject(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "get_facts_for_subject",
		Description: "Return every persisted semantic fact about a subject. Subjects are " +
			"typically a project's cwd; predicates pick from a small recommended vocabulary " +
			"(uses_language_version, runs_tests_via, runs_build_via, key_directory, ...). " +
			"Use to recall what the agent has learned about a project across past sessions.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"subject": {"type": "string", "description": "exact subject (typically a cwd)"},
				"limit":   {"type": "integer", "minimum": 1, "maximum": 200, "default": 50}
			},
			"required": ["subject"]
		}`),
		Handler: getFactsForSubjectAPIHandler(c),
	})
}

func getFactsForSubjectAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Subject string `json:"subject"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "get_facts_for_subject: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Subject) == "" {
			return TextError("get_facts_for_subject: subject is required"), nil
		}
		if req.Limit <= 0 || req.Limit > 200 {
			req.Limit = 50
		}
		resp, err := c.Facts(ctx, req.Subject, req.Limit)
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_facts_for_subject: load: " + err.Error()}
		}
		if len(resp.Facts) == 0 {
			return TextResult(fmt.Sprintf(
				"(no facts known for %q yet — try `aichronicles facts induce --session <id>` on a past session in this project)",
				req.Subject)), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "subject: %s\n", req.Subject)
		for _, f := range resp.Facts {
			fmt.Fprintf(&b, "%s\t%s\t%.2f\t%s\n",
				f.Predicate, f.Object, f.Confidence,
				formatTS(f.AssertedAtMs))
			if f.EvidenceQuote != nil && *f.EvidenceQuote != "" {
				fmt.Fprintf(&b, "  quote: %s\n", *f.EvidenceQuote)
			}
		}
		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

// --- find_fact_subjects ---

func registerFindFactSubjects(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "find_fact_subjects",
		Description: "Find subjects of stored facts whose name contains a substring (case-insensitive). " +
			"Use to discover which projects (cwds) the agent has captured semantic facts about, " +
			"before calling get_facts_for_subject.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"contains": {"type": "string", "description": "substring of the subject; required"},
				"limit":    {"type": "integer", "minimum": 1, "maximum": 100, "default": 30}
			},
			"required": ["contains"]
		}`),
		Handler: findFactSubjectsAPIHandler(c),
	})
}

func findFactSubjectsAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Contains string `json:"contains"`
			Limit    int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "find_fact_subjects: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Contains) == "" {
			return TextError("find_fact_subjects: contains is required"), nil
		}
		if req.Limit <= 0 || req.Limit > 100 {
			req.Limit = 30
		}
		resp, err := c.FactSubjects(ctx, req.Contains, req.Limit)
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "find_fact_subjects: load: " + err.Error()}
		}
		if len(resp.Subjects) == 0 {
			return TextResult("(no fact subjects matched)"), nil
		}
		return TextResult(strings.Join(resp.Subjects, "\n")), nil
	}
}

// --- get_skill_staleness ---

func registerGetSkillStaleness(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "get_skill_staleness",
		Description: "Detect skills whose instructions may be wrong or outdated. " +
			"Returns Claude Code skills where loading them is correlated with a subsequent tool_failure " +
			"in the same session within ~10 minutes — a strong hint that the skill's steps don't work " +
			"on the current environment anymore. " +
			"Use when the user asks 'are any of my skills broken', 'which skills should I fix', " +
			"or after they hit an unexpected failure while using a skill.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"since_days":     {"type": "integer", "minimum": 1, "maximum": 365, "default": 14},
				"window_minutes": {"type": "integer", "minimum": 1, "maximum": 240, "default": 10}
			}
		}`),
		Handler: getSkillStalenessAPIHandler(c),
	})
}

func getSkillStalenessAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			SinceDays     int `json:"since_days"`
			WindowMinutes int `json:"window_minutes"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "get_skill_staleness: bad args: " + err.Error()}
			}
		}
		days := req.SinceDays
		if days <= 0 || days > 365 {
			days = 14
		}
		windowMs := int64(req.WindowMinutes) * 60 * 1000
		if windowMs <= 0 {
			windowMs = 10 * 60 * 1000
		}
		sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

		resp, err := c.SkillStaleness(ctx, api.SkillStalenessRequest{
			SinceMs:  sinceMs,
			WindowMs: windowMs,
		})
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_skill_staleness: " + err.Error()}
		}
		if len(resp.Skills) == 0 {
			return TextResult(fmt.Sprintf("No stale-correlated skills in the last %d days.", days)), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Stale-candidate skills — last %d days, failure window %dm:\n\n",
			days, windowMs/(60*1000))
		for _, r := range resp.Skills {
			fmt.Fprintf(&b, "  %s  stale=%d/%d (%.0f%%)\n",
				r.Name, r.StaleLoads, r.TotalLoads, r.Rate*100)
			for _, ex := range r.Examples {
				short := ex
				if len(short) > 8 {
					short = short[:8]
				}
				fmt.Fprintf(&b, "    example: %s\n", short)
			}
		}
		return TextResult(b.String()), nil
	}
}

// --- get_insights ---

func registerGetInsights(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "get_insights",
		Description: "Cross-session usage report over the last N days of the user's Claude Code / Gemini CLI " +
			"history: total sessions / events / tool calls, top tools (with %), top skills, " +
			"activity-by-hour heatmap, busiest sessions. " +
			"Use when the user asks 'what tools have I been using', 'how many sessions this month', " +
			"'when do I usually work', or wants a usage-pattern overview. " +
			"Pure SQL aggregation; fast, no LLM call.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"since_days":  {"type": "integer", "minimum": 1, "maximum": 365, "default": 30},
				"top_tools":   {"type": "integer", "minimum": 1, "maximum": 50, "default": 15},
				"top_skills":  {"type": "integer", "minimum": 1, "maximum": 50, "default": 10}
			}
		}`),
		Handler: getInsightsAPIHandler(c),
	})
}

func getInsightsAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			SinceDays int `json:"since_days"`
			TopTools  int `json:"top_tools"`
			TopSkills int `json:"top_skills"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "get_insights: bad args: " + err.Error()}
			}
		}
		days := req.SinceDays
		if days <= 0 || days > 365 {
			days = 30
		}
		sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

		resp, err := c.Insights(ctx, apiclient.InsightsRequest{
			SinceMs:   sinceMs,
			TopTools:  req.TopTools,
			TopSkills: req.TopSkills,
		})
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_insights: " + err.Error()}
		}
		return TextResult(formatInsightsAPI(&resp, days)), nil
	}
}

// formatInsightsAPI renders an api.Insights value in the same
// shape formatInsightsForMCP does for the legacy *store.InsightsReport.
// Lives here so internal/mcp doesn't depend on internal/store for
// the migrated handler's rendering.
func formatInsightsAPI(r *api.Insights, days int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Insights — last %d days (%s → %s)\n\n",
		days,
		time.UnixMilli(r.Window.SinceMs).UTC().Format("2006-01-02"),
		time.UnixMilli(r.Window.UntilMs).UTC().Format("2006-01-02"),
	)
	if r.Overview.Sessions == 0 {
		b.WriteString("(no sessions in window)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Overview: %d sessions / %d events / %d tool calls (%d distinct tools) / %d skills\n\n",
		r.Overview.Sessions, r.Overview.Events, r.Overview.ToolUses,
		r.Overview.DistinctTools, r.Overview.DistinctSkills)

	if len(r.TopTools) > 0 {
		b.WriteString("Top tools:\n")
		for _, t := range r.TopTools {
			pct := 0.0
			if r.Overview.ToolUses > 0 {
				pct = 100 * float64(t.Count) / float64(r.Overview.ToolUses)
			}
			fmt.Fprintf(&b, "  %s\t%d\t%.1f%%\n", t.ToolName, t.Count, pct)
		}
		b.WriteByte('\n')
	}
	if len(r.TopSkills) > 0 {
		b.WriteString("Top skills:\n")
		for _, sk := range r.TopSkills {
			fmt.Fprintf(&b, "  %s\t%d\n", sk.Name, sk.Count)
		}
		b.WriteByte('\n')
	}
	if len(r.TopSessions) > 0 {
		b.WriteString("Top sessions (by event count):\n")
		for _, ts := range r.TopSessions {
			short := ts.SessionID
			if len(short) > 8 {
				short = short[:8]
			}
			cwd := "-"
			if ts.Cwd != nil && *ts.Cwd != "" {
				cwd = *ts.Cwd
			}
			fmt.Fprintf(&b, "  %s  %d events  %s\n", short, ts.EventCount, cwd)
		}
	}
	return b.String()
}
