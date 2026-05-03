package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// RegisterAichroniclesTools adds the three read-only tools Block C
// ships (search_events, list_sessions, get_summary). Callers that
// want a different set can register their own without touching this.
//
// All tools read through *store.Store — no privileged writes. An
// MCP client that compromises its sandbox still only reads a subset
// of already-stored events, already scrubbed at events.
func RegisterAichroniclesTools(s *Server, st *store.Store) {
	// search_events is registered by RegisterAichroniclesAPITools.

	// list_sessions, get_summary, and list_workflows are
	// registered by RegisterAichroniclesAPITools.
	// find_episodes, list_subagents, get_unresolved_for_cwd,
	// get_facts_for_subject, and find_fact_subjects are too.

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
		Handler: getProjectContextHandler(st),
	})
}

// search_events migrated to tools_apiclient.go.

// list_sessions, get_summary migrated to tools_apiclient.go.

// --- get_project_context ---

func getProjectContextHandler(st *store.Store) ToolHandler {
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

		// Section 1: Recent sessions in this cwd. Reuses
		// list_sessions's query shape but inlined so the response
		// stays a single text block. cwd-exact match — no prefix
		// matching, same convention as list_sessions / unresolved.
		if err := renderRecentSessionsForCwd(ctx, st, &b, req.Cwd, req.MaxPerSection); err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: sessions: " + err.Error()}
		}

		// Section 2: Open unresolved items. Same source as
		// get_unresolved_for_cwd; capped at max_per_section sessions
		// and per-session items so the prompt stays compact.
		items, err := store.LoadUnresolvedForCwd(ctx, st.DB(), req.Cwd, sinceMs, req.MaxPerSection, req.MaxPerSection)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: unresolved: " + err.Error()}
		}
		renderUnresolvedSection(&b, items)

		// Section 3: Typed semantic facts. Subject is the cwd
		// verbatim (the v1 fact-subject convention).
		facts, err := store.LoadFactsForSubject(ctx, st.DB(), req.Cwd, req.MaxPerSection*4)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: facts: " + err.Error()}
		}
		renderFactsSection(&b, facts)

		// Section 4: Recent workflows. Workflows are project-
		// agnostic by design (the abstraction is the point) — show
		// the most recent N as task-shape candidates the agent can
		// scan for relevance to its current task. Round 8: workflows
		// live inside kind=induction rows (in body.workflow), not in
		// their own kind — pull induction rows and let
		// renderWorkflowsSection filter for those with a workflow.
		wfs, err := store.LoadLLMOutputs(ctx, st.DB(), store.LLMOutputFilter{
			Kind:  store.LLMKindInduction,
			Limit: req.MaxPerSection * 3,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_project_context: workflows: " + err.Error()}
		}
		renderWorkflowsSection(&b, wfs, req.MaxPerSection)

		// Section 5: Installed skills (global + project-local).
		// Skills are project-aware via internal/skills.CollectInstalled
		// — it discovers .claude/skills/ under each known cwd. List
		// just the names so the section stays compact; the agent
		// can call list_skills (analytics) for the full table.
		installed, ierr := skills.CollectInstalled(ctx, st.DB(), sinceMs)
		if ierr != nil {
			// Best-effort; the rest of the context is still useful.
			fmt.Fprintf(&b, "\n## Skills installed\n(skill discovery failed: %v)\n", ierr)
		} else {
			renderSkillsSection(&b, installed, req.MaxPerSection*2)
		}

		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

func renderRecentSessionsForCwd(ctx context.Context, st *store.Store, b *strings.Builder, cwd string, limit int) error {
	rows, err := st.DB().QueryContext(ctx,
		`SELECT s.id, s.started_at_ms, s.ended_at_ms, s.event_count,
		        s.first_prompt_text, s.summary_topic
		   FROM sessions s
		  WHERE s.cwd = ?
		  ORDER BY `+store.EffectiveTsExpr+` DESC
		  LIMIT ?`,
		cwd, limit)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	fmt.Fprintf(b, "\n## Recent sessions in this cwd\n")
	any := false
	for rows.Next() {
		var id string
		var startedMs, endedMs sql.NullInt64
		var eventCount int
		var firstPrompt, topic sql.NullString
		if err := rows.Scan(&id, &startedMs, &endedMs, &eventCount, &firstPrompt, &topic); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		any = true
		title := nullOrDash(topic)
		if title == "-" {
			title = oneLineSnippet(firstPrompt)
		}
		fmt.Fprintf(b, "- %s  %s  %d events  %s\n",
			first8(id), formatTSNullable(endedMs), eventCount, title)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !any {
		fmt.Fprintln(b, "(none — this is the first session in this cwd)")
	}
	return nil
}

func renderUnresolvedSection(b *strings.Builder, items []store.UnresolvedItem) {
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

func renderFactsSection(b *strings.Builder, facts []store.SemanticFact) {
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

// renderWorkflowsSection writes the workflows-extracted-so-far list,
// drawn from kind=induction llm_outputs rows whose body has a
// non-null `workflow` field. (After Round 8, workflows are emitted
// inline by the unified record_induction tool — there is no
// separate kind=workflow row anymore.)
func renderWorkflowsSection(b *strings.Builder, rows []store.LLMOutput, limit int) {
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
		// One-line procedure preview so the agent can decide
		// without fetching the full workflow.
		if len(w.Procedure) > 0 {
			steps := make([]string, 0, len(w.Procedure))
			for _, s := range w.Procedure {
				steps = append(steps, s.Action)
			}
			procPreview := strings.Join(steps, " → ")
			const maxRunes = 200
			r := []rune(procPreview)
			if len(r) > maxRunes {
				procPreview = string(r[:maxRunes]) + "…"
			}
			fmt.Fprintf(b, "  procedure: %s\n", procPreview)
		}
		rendered++
	}
	if rendered == 0 {
		fmt.Fprintln(b, "(none — try `aichronicles induction sweep` to populate the workflow corpus)")
	}
}

func renderSkillsSection(b *strings.Builder, skills []prompts.InstalledSkill, limit int) {
	fmt.Fprintf(b, "\n## Skills installed\n")
	if len(skills) == 0 {
		fmt.Fprintln(b, "(none discovered under ~/.claude/skills or any known project root)")
		return
	}
	rendered := 0
	for _, sk := range skills {
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
	if len(skills) > rendered {
		fmt.Fprintf(b, "  (… %d more installed; call list_skills for the full list)\n",
			len(skills)-rendered)
	}
}

// get_facts_for_subject and find_fact_subjects are now in
// tools_apiclient.go (registered by RegisterAichroniclesAPITools).

// list_workflows migrated to tools_apiclient.go.

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

// --- helpers ---

func first8(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

func formatTS(ms int64) string {
	return timefmt.AbsoluteRFC3339(ms)
}

func formatTSNullable(n sql.NullInt64) string {
	return timefmt.AbsoluteRFC3339OrDash(n)
}

func nullOrDash(s sql.NullString) string { return nullable.OrDash(s) }

// oneLineSnippet flattens whitespace and caps a content preview so
// each tool row stays on a single terminal line. Wraps
// internal/preview so the rune cap matches the web's
// truncatePreview and the CLI snippet renderers.
func oneLineSnippet(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	return preview.OneLine(s.String)
}
