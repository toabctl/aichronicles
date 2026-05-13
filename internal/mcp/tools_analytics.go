package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/timefmt"
)

// RegisterAichroniclesAnalyticsTools wires up the LLM-free analytics
// tools that don't fit cleanly under RegisterAichroniclesAPITools
// because they mix filesystem discovery with api reads.
//
// list_skills is the only resident here today — it walks the
// filesystem for SKILL.md inventory (global ~/.claude/skills/ +
// project-local <project>/.claude/skills/) and queries
// /v1/skills/invoked for invocation counts. The split is necessary
// because skill bodies live on disk; the api can't read them
// without an upload step we don't have.
func RegisterAichroniclesAnalyticsTools(s *Server, c *apiclient.Client) {
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
				"cwd":        {"type": "string", "description": "Optional working-directory path. When set, project-local skills are limited to this cwd; when unset, only global skills are listed."},
				"since_days": {"type": "integer", "minimum": 1, "maximum": 365, "default": 30}
			}
		}`),
		Handler: listSkillsHandler(c),
	})
}

func listSkillsHandler(c *apiclient.Client) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Cwd       string `json:"cwd"`
			SinceDays int    `json:"since_days"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, &Error{Code: InvalidParams, Message: "list_skills: bad args: " + err.Error()}
			}
		}
		sinceMs, days := timefmt.SinceMsFromDays(req.SinceDays, 30, 365, time.Now())

		// Filesystem-side: SKILL.md inventory. cwd-scoped — global
		// SKILL.md plus the named project's .claude/skills/ when
		// req.Cwd is set. Without cwd we get global-only, which is
		// the safe default for an MCP server invoked without
		// project context.
		installed := skills.CollectInstalledForCwd(req.Cwd)

		// Api-side: invocation counts derived from skill_load
		// extractions in the window.
		invokedResp, err := c.InvokedSkills(ctx, sinceMs)
		if err != nil {
			if errors.Is(err, apiclient.ErrSocketUnavailable) {
				return TextError("list_skills: aichronicles-api unreachable: %v", err), nil
			}
			return nil, &Error{Code: InternalError, Message: "list_skills: invoked: " + err.Error()}
		}
		invoked := invokedResp.Skills

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
