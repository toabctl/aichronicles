package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/wire"
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
// happens to be on. A tool MUST appear in exactly one registry;
// RegisterTool panics on duplicate names so the conflict surfaces
// at startup rather than silently shadowing one handler with
// another.
func RegisterAichroniclesAPITools(s *Server, c *apiclient.Client) {
	registerGetUnresolvedForCwd(s, c)
	registerGetFactsForSubject(s, c)
	registerFindFactSubjects(s, c)
	registerGetSkillStaleness(s, c)
	registerGetInsights(s, c)
	registerFindEpisodes(s, c)
	registerListSubagents(s, c)
	registerSearchEvents(s, c)
	registerListSessions(s, c)
	registerGetSummary(s, c)
	registerListWorkflows(s, c)
	registerGetProjectContext(s, c)
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

		resp, err := c.SkillStaleness(ctx, wire.SkillStalenessRequest{
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
				fmt.Fprintf(&b, "    example: %s\n", preview.ShortID(ex))
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

// formatInsightsAPI renders an wire.Insights value in the same
// shape formatInsightsForMCP does for the legacy *store.InsightsReport.
// Lives here so internal/mcp doesn't depend on internal/store for
// the migrated handler's rendering.
func formatInsightsAPI(r *wire.Insights, days int) string {
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
			short := preview.ShortID(ts.SessionID)
			cwd := "-"
			if ts.Cwd != nil && *ts.Cwd != "" {
				cwd = *ts.Cwd
			}
			fmt.Fprintf(&b, "  %s  %d events  %s\n", short, ts.EventCount, cwd)
		}
	}
	return b.String()
}

// --- find_episodes ---

func registerFindEpisodes(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "find_episodes",
		Description: "Find episodic memories — bounded, contextually-coherent slices of past " +
			"sessions where the user (or agent) pursued one intent. Each episode is keyed by its " +
			"intent_summary, the first user prompt that opened it. " +
			"Use when the user asks 'when did I last try to X', 'show me the time we worked on Y', " +
			"or wants concrete prior trajectories rather than aggregate stats.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":      {"type": "string",  "description": "case-insensitive substring of the intent_summary"},
				"cwd":        {"type": "string",  "description": "exact cwd match"},
				"session_id": {"type": "string",  "description": "narrow to one session; accepts an 8+ char prefix"},
				"since_days": {"type": "integer", "minimum": 1, "maximum": 365},
				"limit":      {"type": "integer", "minimum": 1, "maximum": 100, "default": 50}
			}
		}`),
		Handler: findEpisodesAPIHandler(c),
	})
}

func findEpisodesAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Query     string `json:"query"`
			Cwd       string `json:"cwd"`
			SessionID string `json:"session_id"`
			SinceDays int    `json:"since_days"`
			Limit     int    `json:"limit"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "find_episodes: bad args: " + err.Error()}
			}
		}
		if req.Limit <= 0 || req.Limit > 100 {
			req.Limit = 50
		}
		var sinceMs int64
		if req.SinceDays > 0 {
			sinceMs = time.Now().Add(-time.Duration(req.SinceDays) * 24 * time.Hour).UnixMilli()
		}

		// Accept short prefixes for session_id like list_sessions
		// emits. Resolve to canonical id via the api so the
		// underlying Episodes filter matches.
		sessionID := req.SessionID
		if sessionID != "" {
			full, err := c.ResolveSession(ctx, sessionID)
			if err != nil {
				if errors.Is(err, apiclient.ErrSocketUnavailable) {
					return TextError("aichronicles-api unreachable; is the daemon running?"), nil
				}
				return TextError("find_episodes: %v", err), nil
			}
			sessionID = full
		}

		resp, err := c.Episodes(ctx, wire.EpisodeListRequest{
			SessionID:     sessionID,
			Cwd:           req.Cwd,
			QueryContains: req.Query,
			SinceMs:       sinceMs,
			Limit:         req.Limit,
		})
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "find_episodes: query: " + err.Error()}
		}
		if len(resp.Episodes) == 0 {
			return TextResult("(no episodes)"), nil
		}
		var b strings.Builder
		for _, ep := range resp.Episodes {
			cwd := "-"
			if ep.Cwd != nil && *ep.Cwd != "" {
				cwd = *ep.Cwd
			}
			intent := ep.IntentSummary
			if intent == "" {
				intent = "-"
			}
			short := preview.ShortID(ep.SessionID)
			fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\t%s\n",
				short, ep.Ordinal,
				formatTS(ep.StartedAtMs),
				formatTS(ep.EndedAtMs),
				cwd, intent)
		}
		return TextResult(b.String()), nil
	}
}

// --- list_subagents ---

func registerListSubagents(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "list_subagents",
		Description: "List sub-agent threads. Optional session_id (accepts 8+ char prefix) narrows " +
			"to one session; absent, returns recent spans across the whole store. " +
			"Use to discover subagent_id values for search_events filtering.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string"},
				"limit":      {"type": "integer", "minimum": 1, "maximum": 100, "default": 50}
			}
		}`),
		Handler: listSubagentsAPIHandler(c),
	})
}

func listSubagentsAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			SessionID string `json:"session_id"`
			Limit     int    `json:"limit"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "list_subagents: bad args: " + err.Error()}
			}
		}
		if req.Limit <= 0 || req.Limit > 100 {
			req.Limit = 50
		}

		sessionID := req.SessionID
		if sessionID != "" {
			full, err := c.ResolveSession(ctx, sessionID)
			if err != nil {
				if errors.Is(err, apiclient.ErrSocketUnavailable) {
					return TextError("aichronicles-api unreachable; is the daemon running?"), nil
				}
				return TextError("list_subagents: %v", err), nil
			}
			sessionID = full
		}

		resp, err := c.SubagentSpans(ctx, sessionID, req.Limit)
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "list_subagents: query: " + err.Error()}
		}
		if len(resp.Spans) == 0 {
			return TextResult("(no subagent threads)"), nil
		}
		var b strings.Builder
		for _, sp := range resp.Spans {
			short := preview.ShortID(sp.SessionID)
			subType := "-"
			if sp.SubagentType != nil && *sp.SubagentType != "" {
				subType = *sp.SubagentType
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%d\n",
				short, sp.SubagentID, subType,
				formatTS(sp.StartedAtMs),
				formatTS(sp.EndedAtMs),
				sp.EventCount)
		}
		return TextResult(b.String()), nil
	}
}

// --- search_events ---

func registerSearchEvents(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "search_events",
		Description: "Search the user's PAST Claude Code and Gemini CLI sessions by keyword. " +
			"Returns matching events with session id, timestamp, kind, and a snippet centred on " +
			"the match. Use when the user asks 'when did I…?', 'find the session where…', " +
			"'did I work on…'. The corpus is every captured hook event from past sessions, " +
			"indexed by SQLite FTS5; this is the user's actual conversation history, not a " +
			"generic web search. " +
			"Bare tokens match by prefix (mongo finds mongodb); wrap exact matches in double " +
			"quotes (\"panic stack\").",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":        {"type": "string", "description": "Search words. Bare tokens match by prefix; wrap exact matches in double quotes."},
				"subagent_id":  {"type": "string", "description": "Narrow to events run inside one sub-agent thread; pair with list_subagents to discover ids."},
				"limit":        {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}
			},
			"required": ["query"]
		}`),
		Handler: searchEventsAPIHandler(c),
	})
}

func searchEventsAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Query      string `json:"query"`
			SubagentID string `json:"subagent_id"`
			Limit      int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "search_events: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Query) == "" {
			return TextError("search_events: query is required"), nil
		}
		if req.Limit <= 0 || req.Limit > 100 {
			req.Limit = 20
		}

		// The api parses the user-facing query through
		// internal/searchquery server-side and surfaces
		// ErrSyntax as a 400 problem+json. Translate that back
		// to a user-friendly TextError so the agent sees the
		// hint rather than an opaque "400 Invalid q".
		resp, err := c.Search(ctx, wire.SearchRequest{
			Q:          req.Query,
			SubagentID: req.SubagentID,
			Limit:      req.Limit,
		})
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			var herr *apiclient.HTTPError
			if errors.As(err, &herr) && herr.Status == 400 {
				return TextError("search_events: %s", herr.Problem.Detail), nil
			}
			return nil, &Error{Code: InternalError, Message: "search_events: query: " + err.Error()}
		}

		// search_events historically returned "no events for
		// subagent_id" rather than an empty list when the
		// subagent didn't exist — the api can't distinguish
		// "no events match" from "subagent doesn't exist", so
		// the agent gets a generic empty result. Acceptable
		// regression: the cost of dropping the
		// store.SubagentExists pre-check is one less round-trip
		// to a typo-checking endpoint.
		if len(resp.Hits) == 0 {
			return TextResult("(no hits)"), nil
		}
		var b strings.Builder
		for _, h := range resp.Hits {
			short := preview.ShortID(h.SessionID)
			snippet := ""
			if h.Snippet != nil && *h.Snippet != "" {
				snippet = *h.Snippet
			} else if h.Content != nil {
				snippet = *h.Content
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n",
				short, formatTS(h.TsSourceMs), h.Kind, oneLineSnippet2(snippet))
		}
		return TextResult(b.String()), nil
	}
}

// oneLineSnippet2 mirrors oneLineSnippet but takes a plain
// string (api hits already projected the nullable). Kept local
// to the api handlers so internal/store types don't leak in.
func oneLineSnippet2(s string) string {
	const maxRunes = 200
	cleaned := strings.Join(strings.Fields(s), " ")
	if len(cleaned) > maxRunes {
		cleaned = cleaned[:maxRunes] + "…"
	}
	return cleaned
}

// --- list_sessions ---

func registerListSessions(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "list_sessions",
		Description: "List the user's recent past Claude Code / Gemini CLI conversations, newest first. " +
			"Each row is one session: id, started/ended time, working directory, event count. " +
			"Use when the user asks 'what was I doing yesterday', 'show me recent sessions'. " +
			"For keyword search, use search_events instead.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cwd":        {"type": "string",  "description": "exact cwd match"},
				"since_hours":{"type": "integer", "minimum": 1, "description": "limit to sessions ended within this many hours"},
				"limit":      {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}
			}
		}`),
		Handler: listSessionsAPIHandler(c),
	})
}

func listSessionsAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Cwd        string `json:"cwd"`
			SinceHours int    `json:"since_hours"`
			Limit      int    `json:"limit"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "list_sessions: bad args: " + err.Error()}
			}
		}
		if req.Limit <= 0 || req.Limit > 100 {
			req.Limit = 20
		}
		var sinceMs int64
		if req.SinceHours > 0 {
			sinceMs = time.Now().Add(-time.Duration(req.SinceHours) * time.Hour).UnixMilli()
		}
		resp, err := c.Sessions(ctx, wire.SessionListRequest{
			Cwd:     req.Cwd,
			SinceMs: sinceMs,
			Limit:   req.Limit,
		})
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "list_sessions: " + err.Error()}
		}
		if len(resp.Sessions) == 0 {
			return TextResult("(no sessions)"), nil
		}
		var b strings.Builder
		for _, ss := range resp.Sessions {
			short := preview.ShortID(ss.ID)
			started := "-"
			ended := "-"
			if ss.StartedAtMs != nil && *ss.StartedAtMs > 0 {
				started = formatTS(*ss.StartedAtMs)
			}
			if ss.EndedAtMs != nil && *ss.EndedAtMs > 0 {
				ended = formatTS(*ss.EndedAtMs)
			}
			cwd := "-"
			if ss.Cwd != nil && *ss.Cwd != "" {
				cwd = *ss.Cwd
			}
			fp := ""
			if ss.FirstPrompt != nil {
				fp = *ss.FirstPrompt
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\t%s\n",
				short, started, ended, ss.EventCount, cwd, oneLineSnippet2(fp))
		}
		return TextResult(b.String()), nil
	}
}

// --- get_summary ---

func registerGetSummary(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "get_summary",
		Description: "Fetch the cached LLM-generated summary of one past session. " +
			"Returns the structured summary body if one was generated. " +
			"Pass kind=reflect or kind=propose for the multi-session analysis kinds.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string"},
				"kind":       {"type": "string", "enum": ["summary", "reflect", "propose"], "default": "summary"}
			},
			"required": ["session_id"]
		}`),
		Handler: getSummaryAPIHandler(c),
	})
}

func getSummaryAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			SessionID string `json:"session_id"`
			Kind      string `json:"kind"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "get_summary: bad args: " + err.Error()}
		}
		if req.SessionID == "" {
			return TextError("get_summary: session_id is required"), nil
		}
		kind := req.Kind
		if kind == "" {
			kind = "summary"
		}

		// Resolve short prefixes to canonical id.
		full, err := c.ResolveSession(ctx, req.SessionID)
		if err != nil {
			if errors.Is(err, apiclient.ErrNotFound) {
				return TextError("get_summary: no session matches %q", req.SessionID), nil
			}
			if errors.Is(err, apiclient.ErrConflict) {
				return TextError("get_summary: prefix %q is ambiguous", req.SessionID), nil
			}
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_summary: resolve: " + err.Error()}
		}

		outs, err := c.SessionLLMOutputs(ctx, full, kind, 1)
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_summary: " + err.Error()}
		}
		if len(outs) == 0 {
			return TextError("no %s output for session %s", kind, full), nil
		}
		// Newest first; take the first.
		return TextResult(outs[0].Body), nil
	}
}

// --- list_workflows ---

func registerListWorkflows(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "list_workflows",
		Description: "List abstract procedural workflows aichronicles has induced from past " +
			"sessions (AWM — Agent Workflow Memory). Each workflow is a task_shape (abstract " +
			"description) plus a numbered procedure of NL action steps with {placeholder} tokens " +
			"for varying values. " +
			"Use when the user is about to start a task and you want to check whether a similar " +
			"task shape has been done before. " +
			"Pass `task_shape_contains` to narrow by substring (case-insensitive).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task_shape_contains": {"type": "string"},
				"limit":               {"type": "integer", "minimum": 1, "maximum": 50, "default": 10},
				"include_not_found":   {"type": "boolean", "default": false}
			}
		}`),
		Handler: listWorkflowsAPIHandler(c),
	})
}

func listWorkflowsAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			TaskShapeContains string `json:"task_shape_contains"`
			Limit             int    `json:"limit"`
			IncludeNotFound   bool   `json:"include_not_found"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "list_workflows: bad args: " + err.Error()}
			}
		}
		if req.Limit <= 0 || req.Limit > 50 {
			req.Limit = 10
		}
		// Pull more than the cap because most induction rows
		// have no workflow — filter post-fetch.
		outs, err := c.LLMOutputsList(ctx, "induction", "", req.Limit*5)
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("aichronicles-api unreachable; is the daemon running?"), nil
			}
			return nil, &Error{Code: InternalError, Message: "list_workflows: " + err.Error()}
		}

		needle := strings.ToLower(strings.TrimSpace(req.TaskShapeContains))
		type entry struct {
			row wire.LLMOutput
			ind prompts.InductionResult
		}
		var keep []entry
		for _, r := range outs {
			var ind prompts.InductionResult
			if jerr := json.Unmarshal([]byte(r.Body), &ind); jerr != nil {
				continue
			}
			if ind.Workflow == nil {
				if !req.IncludeNotFound {
					continue
				}
				keep = append(keep, entry{row: r, ind: ind})
				if len(keep) >= req.Limit {
					break
				}
				continue
			}
			if needle != "" && !strings.Contains(strings.ToLower(ind.Workflow.TaskShape), needle) {
				continue
			}
			keep = append(keep, entry{row: r, ind: ind})
			if len(keep) >= req.Limit {
				break
			}
		}
		if len(keep) == 0 {
			return TextResult("(no workflows yet — try `aichronicles induction sweep` to populate the workflow corpus)"), nil
		}

		var b strings.Builder
		for _, e := range keep {
			sessShort := "(none)"
			if e.row.SessionID != nil && *e.row.SessionID != "" {
				sessShort = preview.ShortID(*e.row.SessionID)
			}
			when := formatTS(e.row.CreatedAtMs)
			if e.ind.Workflow == nil {
				fmt.Fprintf(&b, "%s\t%s\t(no workflow — %s)\n", sessShort, when, e.ind.Rationale)
				continue
			}
			w := e.ind.Workflow
			fmt.Fprintf(&b, "%s\t%s\t%s\n", sessShort, when, w.TaskShape)
			for i, step := range w.Procedure {
				fmt.Fprintf(&b, "  %d. %s\n", i+1, step.Action)
			}
			if len(w.Preconditions) > 0 {
				fmt.Fprintln(&b, "  preconditions:")
				for _, p := range w.Preconditions {
					fmt.Fprintf(&b, "    - %s\n", p)
				}
			}
		}
		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

// --- get_project_context ---

func registerGetProjectContext(s *Server, c *apiclient.Client) {
	s.RegisterTool(Tool{
		Name: "get_project_context",
		Description: "Single-call session-start orientation for a working directory. Returns " +
			"every memory layer the agent needs to ground itself in a project the user has " +
			"worked in before: recent sessions in this cwd, open unresolved threads, typed " +
			"semantic facts (build/test/deploy contract), recent reusable workflows, and " +
			"installed skills. " +
			"Use FIRST in a new session when the user is in a project they have history in — " +
			"before running shell commands to discover go.mod / package.json / pytest config, " +
			"check what's already been induced. " +
			"Distinct from list_sessions / get_unresolved_for_cwd / get_facts_for_subject / " +
			"list_workflows — those each return one slice; this returns the whole context as " +
			"one structured payload, so the agent makes ONE tool call instead of four. " +
			"Empty sections are normal — a fresh project has no facts, no unresolved, no " +
			"prior sessions; the empty-state messages explain how to populate each.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cwd":         {"type": "string", "description": "Absolute working-directory path. Exact match (no prefix)."},
				"since_days":  {"type": "integer", "minimum": 1, "default": 30, "description": "Time window for sessions / unresolved items / installed-skill discovery."},
				"max_per_section": {"type": "integer", "minimum": 1, "maximum": 20, "default": 5, "description": "Cap on entries per section so the result stays scannable."}
			},
			"required": ["cwd"]
		}`),
		Handler: getProjectContextAPIHandler(c),
	})
}

func getProjectContextAPIHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Cwd           string `json:"cwd"`
			SinceDays     int    `json:"since_days"`
			MaxPerSection int    `json:"max_per_section"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "get_project_context: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Cwd) == "" {
			return TextError("get_project_context: cwd is required"), nil
		}
		if req.SinceDays <= 0 {
			req.SinceDays = 30
		}
		if req.MaxPerSection <= 0 || req.MaxPerSection > 20 {
			req.MaxPerSection = 5
		}
		sinceMs := time.Now().Add(-time.Duration(req.SinceDays) * 24 * time.Hour).UnixMilli()

		var b strings.Builder
		fmt.Fprintf(&b, "# Project context: %s\n", req.Cwd)
		fmt.Fprintf(&b, "(window: last %d days; up to %d entries per section)\n",
			req.SinceDays, req.MaxPerSection)

		// Section 1: recent sessions in this cwd. /v1/sessions with
		// the cwd filter returns the same shape MCP list_sessions
		// uses, including event_count.
		if err := renderRecentSessionsForCwdAPI(ctx, c, &b, req.Cwd, sinceMs, req.MaxPerSection); err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("get_project_context: aichronicles-api unreachable: %v", err), nil
			}
			return nil, &Error{Code: InternalError, Message: "get_project_context: sessions: " + err.Error()}
		}

		// Section 2: open unresolved items. Same source as
		// get_unresolved_for_cwd; capped per-cwd and per-session.
		uresp, err := c.Unresolved(ctx, apiclient.UnresolvedRequest{
			Cwd:                req.Cwd,
			SinceMs:            sinceMs,
			MaxSessions:        req.MaxPerSection,
			MaxItemsPerSession: req.MaxPerSection,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: unresolved: " + err.Error()}
		}
		renderUnresolvedSectionAPI(&b, uresp.Items)

		// Section 3: typed semantic facts. Subject is the cwd
		// verbatim (the v1 fact-subject convention).
		fresp, err := c.Facts(ctx, req.Cwd, req.MaxPerSection*4)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: facts: " + err.Error()}
		}
		renderFactsSectionAPI(&b, fresp.Facts)

		// Section 4: recent workflows. Workflows ride inside
		// kind=induction llm_outputs rows (Round 8); pull them via
		// /v1/llm-outputs/list and filter for non-null body.workflow
		// in the renderer.
		wfs, err := c.LLMOutputsList(ctx, "induction", "", req.MaxPerSection*3)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: workflows: " + err.Error()}
		}
		renderWorkflowsSectionAPI(&b, wfs, req.MaxPerSection)

		// Section 5: installed skills. cwd-scoped — global SKILL.md
		// + this project's .claude/skills only. Kept filesystem-side
		// because skill bodies live on disk, not in the api.
		installed := skills.CollectInstalledForCwd(req.Cwd)
		renderSkillsSectionAPI(&b, installed, req.MaxPerSection*2)

		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

func renderRecentSessionsForCwdAPI(ctx context.Context, c *apiclient.Client, b *strings.Builder, cwd string, sinceMs int64, limit int) error {
	resp, err := c.Sessions(ctx, wire.SessionListRequest{
		Cwd:     cwd,
		SinceMs: sinceMs,
		Limit:   limit,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(b, "\n## Recent sessions in this cwd\n")
	if len(resp.Sessions) == 0 {
		fmt.Fprintln(b, "(none — this is the first session in this cwd)")
		return nil
	}
	for _, s := range resp.Sessions {
		title := "-"
		if s.LatestSummary != nil && *s.LatestSummary != "" {
			title = *s.LatestSummary
		} else if s.FirstPrompt != nil && *s.FirstPrompt != "" {
			title = oneLineSnippet2(*s.FirstPrompt)
		}
		ended := "-"
		if s.EndedAtMs != nil {
			ended = formatTS(*s.EndedAtMs)
		}
		fmt.Fprintf(b, "- %s  %s  %d events  %s\n",
			first8(s.ID), ended, s.EventCount, title)
	}
	return nil
}

func renderUnresolvedSectionAPI(b *strings.Builder, items []wire.UnresolvedItem) {
	fmt.Fprintf(b, "\n## Open unresolved threads\n")
	if len(items) == 0 {
		fmt.Fprintln(b, "(none — past sessions wrapped up cleanly)")
		return
	}
	for _, it := range items {
		fmt.Fprintf(b, "- [%s] %s — %s\n",
			it.SessionShort, it.Topic, it.Item)
	}
}

func renderFactsSectionAPI(b *strings.Builder, facts []wire.SemanticFact) {
	fmt.Fprintf(b, "\n## Project facts\n")
	if len(facts) == 0 {
		fmt.Fprintln(b, "(none — try `aichronicles facts induce --session <id>` on a past session in this cwd)")
		return
	}
	for _, f := range facts {
		fmt.Fprintf(b, "- %s = %s  (conf=%.2f)\n",
			f.Predicate, f.Object, f.Confidence)
	}
}

// renderWorkflowsSectionAPI walks wire.LLMOutput rows of kind=induction
// and surfaces those whose body has a non-null workflow field.
// (Round 8 merged workflows into induction rows.)
func renderWorkflowsSectionAPI(b *strings.Builder, rows []wire.LLMOutput, limit int) {
	fmt.Fprintf(b, "\n## Recent workflows (project-agnostic — scan task_shape for relevance)\n")
	rendered := 0
	for _, r := range rows {
		if rendered >= limit {
			break
		}
		var ind prompts.InductionResult
		if err := json.Unmarshal([]byte(r.Body), &ind); err != nil {
			continue
		}
		if ind.Workflow == nil || ind.Workflow.TaskShape == "" {
			continue
		}
		w := ind.Workflow
		fmt.Fprintf(b, "- %s\n", w.TaskShape)
		if len(w.Procedure) > 0 {
			steps := make([]string, 0, len(w.Procedure))
			for _, s := range w.Procedure {
				steps = append(steps, s.Action)
			}
			procPreview := strings.Join(steps, " → ")
			const maxRunes = 200
			rs := []rune(procPreview)
			if len(rs) > maxRunes {
				procPreview = string(rs[:maxRunes]) + "…"
			}
			fmt.Fprintf(b, "  procedure: %s\n", procPreview)
		}
		rendered++
	}
	if rendered == 0 {
		fmt.Fprintln(b, "(none — try `aichronicles induction sweep` to populate the workflow corpus)")
	}
}

func renderSkillsSectionAPI(b *strings.Builder, installed []prompts.InstalledSkill, limit int) {
	fmt.Fprintf(b, "\n## Skills installed\n")
	if len(installed) == 0 {
		fmt.Fprintln(b, "(none discovered under ~/.claude/skills or this project's .claude/skills/)")
		return
	}
	rendered := 0
	for _, sk := range installed {
		if rendered >= limit {
			break
		}
		desc := sk.Description
		const maxDescRunes = 100
		if r := []rune(desc); len(r) > maxDescRunes {
			desc = string(r[:maxDescRunes]) + "…"
		}
		if desc == "" {
			fmt.Fprintf(b, "- %s  (%s)\n", sk.Name, sk.Source)
		} else {
			fmt.Fprintf(b, "- %s  (%s) — %s\n", sk.Name, sk.Source, desc)
		}
		rendered++
	}
	if len(installed) > rendered {
		fmt.Fprintf(b, "  (… %d more installed; call list_skills for the full list)\n",
			len(installed)-rendered)
	}
}
