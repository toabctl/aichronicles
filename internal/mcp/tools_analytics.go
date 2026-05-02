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
	// get_insights and get_skill_staleness are registered by
	// RegisterAichroniclesAPITools (tools_apiclient.go) — they
	// read through the apiclient now.
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
}

// get_insights migrated to tools_apiclient.go.

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

// get_skill_staleness migrated to tools_apiclient.go.
