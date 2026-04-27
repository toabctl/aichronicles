package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
)

// RegisterAichroniclesAnalyticsTools wires up the read-only
// analytics tools the agent can use to introspect its own past
// work: insights, installed/invoked skills, and skill staleness.
//
// Pure SQL paths (no LLM), so they have no API-key dependency
// and slot in alongside the search/list tools that already exist.
// Mirror the data shapes the CLI prints for `aichronicles
// insights`, `aichronicles propose list` (skills section), and
// `aichronicles skills stale`.
func RegisterAichroniclesAnalyticsTools(s *Server, st *store.Store) {
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
		Handler: getInsightsHandler(st),
	})

	s.RegisterTool(Tool{
		Name: "list_skills",
		Description: "List the user's Claude Code skills: which SKILL.md files exist on disk " +
			"(global ~/.claude/skills/ + project-local <project>/.claude/skills/) and which ones " +
			"they have actually invoked recently (with frequency). " +
			"Use when the user asks 'what skills do I have', 'which skills do I actually use', " +
			"or BEFORE proposing a new skill — check whether a similar one already exists. " +
			"Spotting installed-but-never-invoked skills is also useful for cleanup.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"since_days": {"type": "integer", "minimum": 1, "maximum": 365, "default": 30}
			}
		}`),
		Handler: listSkillsHandler(st),
	})

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
		Handler: getSkillStalenessHandler(st),
	})
}

// --- get_insights ---

func getInsightsHandler(st *store.Store) ToolHandler {
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

		report, err := store.LoadInsights(ctx, st.DB(), sinceMs, store.InsightsLimits{
			TopTools:  req.TopTools,
			TopSkills: req.TopSkills,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_insights: " + err.Error()}
		}
		return TextResult(formatInsightsForMCP(report, days)), nil
	}
}

func formatInsightsForMCP(r *store.InsightsReport, days int) string {
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
			if ts.Cwd.Valid && ts.Cwd.String != "" {
				cwd = ts.Cwd.String
			}
			fmt.Fprintf(&b, "  %s  %d events  %s\n", short, ts.EventCount, cwd)
		}
	}
	return b.String()
}

// --- list_skills ---

func listSkillsHandler(st *store.Store) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			SinceDays int `json:"since_days"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "list_skills: bad args: " + err.Error()}
			}
		}
		days := req.SinceDays
		if days <= 0 || days > 365 {
			days = 30
		}
		sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

		installed, err := skills.CollectInstalled(ctx, st.DB(), sinceMs)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "list_skills: collect installed: " + err.Error()}
		}
		invoked, err := skills.LoadInvoked(ctx, st.DB(), sinceMs)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "list_skills: load invoked: " + err.Error()}
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Installed skills (%d):\n", len(installed))
		for _, s := range installed {
			fmt.Fprintf(&b, "  %s [%s]: %s\n", s.Name, s.Source, s.Description)
		}
		if len(installed) == 0 {
			b.WriteString("  (none — neither global ~/.claude/skills/ nor any project-local .claude/skills/ has SKILL.md files)\n")
		}
		fmt.Fprintf(&b, "\nInvoked recently — last %d days (%d distinct):\n", days, len(invoked))
		for _, s := range invoked {
			fmt.Fprintf(&b, "  %s × %d\n", s.Name, s.Count)
		}
		if len(invoked) == 0 {
			b.WriteString("  (none)\n")
		}
		return TextResult(b.String()), nil
	}
}

// --- get_skill_staleness ---

func getSkillStalenessHandler(st *store.Store) ToolHandler {
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

		rows, err := store.LoadSkillStaleness(ctx, st.DB(), sinceMs, windowMs, store.SkillStalenessLimits{})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_skill_staleness: " + err.Error()}
		}
		if len(rows) == 0 {
			return TextResult(fmt.Sprintf("No stale-correlated skills in the last %d days.", days)), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Stale-candidate skills — last %d days, failure window %dm:\n\n",
			days, windowMs/(60*1000))
		for _, r := range rows {
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
