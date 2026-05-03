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

	s.RegisterTool(Tool{
		Name: "list_sessions",
		Description: "List the user's recent past Claude Code / Gemini CLI conversations, newest first. " +
			"Each row is one session: id, started/ended time, working directory, event count. " +
			"Use when the user asks 'what was I doing yesterday', 'show me recent sessions', " +
			"or wants to browse conversation history without a specific search keyword. " +
			"For keyword search, use search_events instead.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cwd":        {"type": "string",  "description": "exact cwd match"},
				"since_hours":{"type": "integer", "minimum": 1, "description": "limit to sessions ended within this many hours"},
				"limit":      {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}
			}
		}`),
		Handler: listSessionsHandler(st),
	})

	// find_episodes is registered by RegisterAichroniclesAPITools.

	s.RegisterTool(Tool{
		Name: "get_summary",
		Description: "Fetch the cached LLM-generated summary of one past Claude Code / Gemini CLI session. " +
			"Returns the structured summary body (topic, what-was-done, unresolved items, key files, links) " +
			"if one was generated. " +
			"Use when the user asks 'what was that session about', 'summarize what we did in <session>', " +
			"or after list_sessions / search_events surfaced a session id worth digesting. " +
			"Not every session has a cached summary — only ones the user ran `aichronicles summarize` on. " +
			"Pass kind=reflect or kind=propose for the multi-session analysis kinds.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string"},
				"kind":       {"type": "string", "enum": ["summary", "reflect", "propose"], "default": "summary"}
			},
			"required": ["session_id"]
		}`),
		Handler: getSummaryHandler(st),
	})

	// list_subagents and get_unresolved_for_cwd are registered by
	// RegisterAichroniclesAPITools (tools_apiclient.go) — those
	// handlers read through internal/apiclient instead of opening
	// the store directly. Production wiring calls both registrars;
	// tests that need either tool spin up the api side via
	// registerAllTools.

	s.RegisterTool(Tool{
		Name: "list_workflows",
		Description: "List abstract procedural workflows aichronicles has induced from past " +
			"sessions (AWM — Agent Workflow Memory). Each workflow is a task_shape (abstract " +
			"description) plus a numbered procedure of NL action steps with {placeholder} tokens " +
			"for varying values. " +
			"Use when the user is about to start a task and you want to check whether a similar " +
			"task shape has been done before — e.g. 'I'm about to deploy to staging, is there a " +
			"workflow for that?'. The agent should scan task_shape values and pick the most " +
			"relevant one to follow as a recipe (substituting values for the {placeholders}). " +
			"Distinct from skills (which are SKILL.md artefacts on disk applied via the Skill " +
			"tool); workflows live only in the database as retrievable exemplars. " +
			"Pass `task_shape_contains` to narrow by substring (case-insensitive). Empty result " +
			"means no workflow has been induced yet — workflows are emitted by the unified " +
			"`aichronicles induction sweep` (or its daemon-resident equivalent) alongside " +
			"skill induction.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task_shape_contains": {"type": "string", "description": "Optional case-insensitive substring filter on task_shape."},
				"limit":               {"type": "integer", "minimum": 1, "maximum": 50, "default": 10},
				"include_not_found":   {"type": "boolean", "default": false, "description": "When true, include workflow rows with found=false (the no-workflow verdicts). Default omits them."}
			}
		}`),
		Handler: listWorkflowsHandler(st),
	})

	// get_facts_for_subject and find_fact_subjects are registered
	// by RegisterAichroniclesAPITools (tools_apiclient.go).

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

// --- list_sessions ---

func listSessionsHandler(st *store.Store) ToolHandler {
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

		var filter strings.Builder
		var sqlArgs []any
		if req.Cwd != "" {
			filter.WriteString(` AND s.cwd = ?`)
			sqlArgs = append(sqlArgs, req.Cwd)
		}
		if req.SinceHours > 0 {
			filter.WriteString(` AND s.ended_at_ms >= ?`)
			sqlArgs = append(sqlArgs, time.Now().Add(-time.Duration(req.SinceHours)*time.Hour).UnixMilli())
		}
		sqlArgs = append(sqlArgs, req.Limit)

		q := `SELECT s.id, s.started_at_ms, s.ended_at_ms, s.event_count, s.cwd,
			s.first_prompt_text AS first_prompt
			FROM sessions s WHERE 1=1` + filter.String() + `
			ORDER BY ` + store.EffectiveTsExpr + ` DESC
			LIMIT ?`

		rows, err := st.DB().QueryContext(ctx, q, sqlArgs...)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "list_sessions: query: " + err.Error()}
		}
		defer func() { _ = rows.Close() }()

		var b strings.Builder
		for rows.Next() {
			var id string
			var startedMs, endedMs sql.NullInt64
			var eventCount int
			var cwd, firstPrompt sql.NullString
			if err := rows.Scan(&id, &startedMs, &endedMs, &eventCount, &cwd, &firstPrompt); err != nil {
				return nil, &Error{Code: InternalError, Message: "list_sessions: scan: " + err.Error()}
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\t%s\n",
				first8(id),
				formatTSNullable(startedMs),
				formatTSNullable(endedMs),
				eventCount,
				nullOrDash(cwd),
				oneLineSnippet(firstPrompt),
			)
		}
		if b.Len() == 0 {
			return TextResult("(no sessions)"), nil
		}
		return TextResult(b.String()), nil
	}
}

// find_episodes and list_subagents migrated to tools_apiclient.go.

// --- get_summary ---

func getSummaryHandler(st *store.Store) ToolHandler {
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
		kind := store.LLMOutputKind(req.Kind)
		if kind == "" {
			kind = store.LLMKindSummary
		}

		sessionID, err := store.ResolveSessionIDPrefix(ctx, st.DB(), req.SessionID)
		if err != nil {
			return TextError("get_summary: %v", err), nil
		}

		outs, err := store.LoadLLMOutputsForSession(ctx, st.DB(), sessionID)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_summary: load: " + err.Error()}
		}
		for _, o := range outs {
			if o.Kind == kind {
				return TextResult(o.Body), nil
			}
		}
		return TextError("no %s output for session %s", kind, sessionID), nil
	}
}

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

// --- list_workflows ---

func listWorkflowsHandler(st *store.Store) ToolHandler {
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

		// After Round 8 workflows are emitted by the unified
		// record_induction call alongside any skill — they live
		// inside kind=induction llm_outputs rows in body.workflow,
		// not in their own kind=workflow rows. Pull more rows than
		// the cap because most induction rows have no workflow
		// (sessions that yielded nothing or only a skill); 5x
		// gives the post-load filter room.
		rows, err := store.LoadLLMOutputs(ctx, st.DB(), store.LLMOutputFilter{
			Kind:  store.LLMKindInduction,
			Limit: req.Limit * 5,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "list_workflows: load: " + err.Error()}
		}

		needle := strings.ToLower(strings.TrimSpace(req.TaskShapeContains))
		type entry struct {
			row store.LLMOutput
			ind prompts.InductionResult
		}
		var keep []entry
		for _, r := range rows {
			var ind prompts.InductionResult
			if jerr := json.Unmarshal([]byte(r.Body), &ind); jerr != nil {
				continue
			}
			// IncludeNotFound surfaces the "no workflow" verdicts —
			// induction rows where the model emitted no workflow.
			// Default omits them since the typical caller wants
			// actionable workflow recipes.
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
			if e.row.SessionID.Valid && len(e.row.SessionID.String) >= 8 {
				sessShort = e.row.SessionID.String[:8]
			}
			when := formatTS(e.row.CreatedAtMs)
			if e.ind.Workflow == nil {
				fmt.Fprintf(&b, "%s\t%s\t(no workflow — %s)\n",
					sessShort, when, e.ind.Rationale)
				continue
			}
			w := e.ind.Workflow
			fmt.Fprintf(&b, "%s\t%s\t%s\n",
				sessShort, when, w.TaskShape)
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
