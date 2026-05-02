package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/pkg/events"
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
	s.RegisterTool(Tool{
		Name: "search_events",
		Description: "Search the user's PAST Claude Code and Gemini CLI sessions by keyword. " +
			"Returns matching events (user prompts, assistant turns, tool calls) with session id, " +
			"timestamp, and a snippet centred on the match. " +
			"Use when the user asks 'when did I…?', 'find the session where…', 'did I work on…', " +
			"or wants to recall a specific prior conversation. " +
			"The corpus is every captured hook event from past sessions, indexed by SQLite FTS5; " +
			"this is the user's actual conversation history, not a generic web search.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":        {"type": "string", "description": "Search words. Bare tokens match by prefix (mongo finds mongodb); wrap exact matches in double quotes (\"panic stack\")."},
				"subagent_id":  {"type": "string", "description": "Narrow to events run inside one sub-agent thread; pair with list_subagents to discover ids."},
				"limit":        {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}
			},
			"required": ["query"]
		}`),
		Handler: searchEventsHandler(st),
	})

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

	s.RegisterTool(Tool{
		Name: "find_episodes",
		Description: "Find episodic memories — bounded, contextually-coherent slices of past " +
			"sessions where the user (or agent) pursued one intent. Each episode is keyed by its " +
			"intent_summary, the first user prompt that opened it. " +
			"Use when the user asks 'when did I last try to X', 'show me the time we worked on Y', " +
			"or wants to recall a SPECIFIC PAST ATTEMPT rather than a session as a whole. " +
			"Distinct from list_sessions (whole sessions) and search_events (raw event hits): " +
			"episodes are the substrate for instance-specific recall — Pink et al. (2026) frame " +
			"this as the episodic layer agents need to retrieve concrete prior trajectories. " +
			"Pass `query` to filter by case-insensitive substring on the intent summary; pass " +
			"`cwd` to scope to one project; pair with get_summary on the returned session_id " +
			"for the full session context. Empty result is normal — sessions only generate " +
			"episodes after the daemon's induction sweep has run on them.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":      {"type": "string",  "description": "Case-insensitive substring to match against the episode's intent summary (the first user prompt that opened it)."},
				"cwd":        {"type": "string",  "description": "Exact-match working directory; narrows to episodes whose first event was in this cwd."},
				"session_id": {"type": "string",  "description": "Narrow to episodes within one session (full UUID; use list_sessions to discover ids)."},
				"since_days": {"type": "integer", "minimum": 1, "maximum": 365, "description": "Only return episodes that ended within this many days."},
				"limit":      {"type": "integer", "minimum": 1, "maximum": 100, "default": 50}
			}
		}`),
		Handler: findEpisodesHandler(st),
	})

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

	s.RegisterTool(Tool{
		Name: "list_subagents",
		Description: "List sub-agent threads from past Claude Code sessions — useful when an Agent or Task " +
			"tool delegated work to a side conversation and the user wants to see what each subagent did. " +
			"Each row is one sub-agent span (started, ended, event count, parent session). " +
			"Use the returned subagent_id as a filter on search_events to read everything that ran inside " +
			"one thread. Only meaningful for sessions that used delegation; most sessions will return zero rows.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string", "description": "narrow to one session (full UUID or unique prefix)"},
				"limit":      {"type": "integer", "minimum": 1, "maximum": 100, "default": 50}
			}
		}`),
		Handler: listSubagentsHandler(st),
	})

	s.RegisterTool(Tool{
		Name: "get_unresolved_for_cwd",
		Description: "List unresolved items that prior sessions left open in the given working " +
			"directory (`cwd`). Reads each prior same-cwd session's latest summary and surfaces " +
			"the entries from its `unresolved` list — the things the previous agent flagged as " +
			"open questions, deferred work, or follow-up TODOs. " +
			"Use when the user opens a session in a project they've worked on before and you " +
			"want to pick up where things were left off, OR when the user explicitly asks " +
			"'what's still open here', 'what was unfinished from last time'. " +
			"Returns one row per item with the source session id, its topic for context, and " +
			"a relative timestamp. Empty result is normal — many sessions wrap up cleanly.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cwd":           {"type": "string",  "description": "absolute path; exact match (no prefix)"},
				"since_days":    {"type": "integer", "minimum": 1, "default": 30, "description": "only consider sessions whose ended_at is within this many days"},
				"max_sessions":  {"type": "integer", "minimum": 1, "maximum": 50, "default": 5, "description": "cap on prior sessions to draw items from"},
				"max_per_session":{"type":"integer", "minimum": 1, "maximum": 20, "default": 5, "description": "cap on items pulled from one session"}
			},
			"required": ["cwd"]
		}`),
		Handler: getUnresolvedForCwdHandler(st),
	})

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

	s.RegisterTool(Tool{
		Name: "get_facts_for_subject",
		Description: "Retrieve typed semantic facts known about one subject (typically a cwd " +
			"path of a project the user has worked in). Returns the (predicate, object, " +
			"confidence) triples derived from past sessions: build/test/deploy contracts, " +
			"language version, key directories, dependencies. " +
			"Use at the START of a session in a project the user has touched before — before " +
			"running shell commands to discover go.mod / package.json / pytest config, " +
			"check whether facts have already been induced. This is the SEMANTIC layer of " +
			"memory (distinct from skills/workflows/episodic search): the answer to 'what " +
			"do I know about this project?'. " +
			"Empty result is normal — facts only exist after `aichronicles facts induce " +
			"--session <id>` has run on a relevant past session.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"subject": {"type": "string", "description": "The subject to load facts for. Typically the project's cwd path."},
				"limit":   {"type": "integer", "minimum": 1, "maximum": 200, "default": 50}
			},
			"required": ["subject"]
		}`),
		Handler: getFactsForSubjectHandler(st),
	})

	s.RegisterTool(Tool{
		Name: "find_fact_subjects",
		Description: "Search the SUBJECTS in the semantic facts store by case-insensitive " +
			"substring. Returns distinct subject strings — useful when the agent has a " +
			"fuzzy idea of which project the user means ('that aichronicles fork') and " +
			"wants to discover the canonical subject string before calling " +
			"get_facts_for_subject. " +
			"Empty result means no semantic facts have been recorded under any subject " +
			"matching the needle.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"contains": {"type": "string", "description": "Case-insensitive substring to match against subject."},
				"limit":    {"type": "integer", "minimum": 1, "maximum": 100, "default": 30}
			},
			"required": ["contains"]
		}`),
		Handler: findFactSubjectsHandler(st),
	})

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

// --- search_events ---

func searchEventsHandler(st *store.Store) ToolHandler {
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

		// Translate the user-facing query (plain words, optionally
		// "quoted phrases") into a syntactically-safe FTS5 MATCH
		// expression. Surface ErrEmpty / ErrSyntax as user-facing
		// tool errors so the agent can correct itself rather than
		// see an opaque SQLite error.
		ftsQuery, err := searchquery.ToFTS5(req.Query)
		if err != nil {
			switch {
			case errors.Is(err, searchquery.ErrEmpty):
				return TextError("search_events: query is required"), nil
			case errors.Is(err, searchquery.ErrSyntax):
				return TextError("search_events: %v", err), nil
			default:
				return nil, &Error{Code: InvalidParams, Message: "search_events: parse query: " + err.Error()}
			}
		}

		// Surface a clear "no such subagent" error rather than
		// silently returning zero hits, which is indistinguishable
		// from "real subagent with no matches" and would hide a
		// typo from the calling agent.
		if req.SubagentID != "" {
			exists, err := store.SubagentExists(ctx, st.DB(), req.SubagentID)
			if err != nil {
				return nil, &Error{Code: InternalError, Message: "search_events: subagent check: " + err.Error()}
			}
			if !exists {
				return TextError("search_events: no events for subagent_id %q", req.SubagentID), nil
			}
		}

		hits, err := store.SearchEvents(ctx, st.DB(), store.SearchEventOpts{
			Query:      ftsQuery,
			SubagentID: req.SubagentID,
			Limit:      req.Limit,
			// MCP defaults to chronological — an agent asking
			// "did I work on X recently?" wants newest first.
			Order: store.OrderRecency,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "search_events: query: " + err.Error()}
		}

		if len(hits) == 0 {
			return TextResult("(no hits)"), nil
		}
		var b strings.Builder
		for _, h := range hits {
			// Prefer SQLite's snippet (centered on the match,
			// tokenizer-aware) over the raw content_text. The
			// trigger keeps it filled for FTS hits; the empty
			// fallback is just defensive.
			preview := h.Snippet
			if !preview.Valid || preview.String == "" {
				preview = h.Content
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n",
				first8(h.SessionID),
				formatTS(h.TsSourceMs),
				h.Kind,
				oneLineSnippet(preview),
			)
		}
		return TextResult(b.String()), nil
	}
}

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

// --- find_episodes ---

func findEpisodesHandler(st *store.Store) ToolHandler {
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
			req.Limit = store.DefaultFindEpisodesLimit
		}
		var sinceMs int64
		if req.SinceDays > 0 {
			sinceMs = time.Now().Add(-time.Duration(req.SinceDays) * 24 * time.Hour).UnixMilli()
		}

		// Resolve a session-id prefix to its full UUID so callers
		// can pass the 8-char preview the tool itself emits below.
		// Mirrors list_subagents — without this, a model that copies
		// the short id back in as session_id silently gets (no
		// episodes).
		sessionID := req.SessionID
		if sessionID != "" {
			full, rerr := store.ResolveSessionIDPrefix(ctx, st.DB(), sessionID)
			if rerr != nil {
				return TextError("find_episodes: %v", rerr), nil
			}
			sessionID = full
		}

		hits, err := store.FindEpisodes(ctx, st.DB(), store.FindEpisodesOpts{
			SessionID:     sessionID,
			Cwd:           req.Cwd,
			QueryContains: req.Query,
			SinceMs:       sinceMs,
			Limit:         req.Limit,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "find_episodes: query: " + err.Error()}
		}
		if len(hits) == 0 {
			return TextResult("(no episodes)"), nil
		}
		var b strings.Builder
		for _, ep := range hits {
			fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\t%s\n",
				first8(ep.SessionID),
				ep.Ordinal,
				formatTS(ep.StartedAtMs),
				formatTS(ep.EndedAtMs),
				nullOrDashEvents(ep.Cwd),
				oneLineSnippet(sql.NullString{String: ep.IntentSummary, Valid: ep.IntentSummary != ""}),
			)
		}
		return TextResult(b.String()), nil
	}
}

// --- list_subagents ---

func listSubagentsHandler(st *store.Store) ToolHandler {
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

		// Resolve a session-id prefix to its full UUID so callers
		// can pass the 8-char preview list_sessions emits.
		sessionID := req.SessionID
		if sessionID != "" {
			full, err := store.ResolveSessionIDPrefix(ctx, st.DB(), sessionID)
			if err != nil {
				return TextError("list_subagents: %v", err), nil
			}
			sessionID = full
		}

		spans, err := store.LoadSubagentSpans(ctx, st.DB(), sessionID, req.Limit)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "list_subagents: query: " + err.Error()}
		}
		if len(spans) == 0 {
			return TextResult("(no subagent threads)"), nil
		}
		var b strings.Builder
		for _, s := range spans {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%d\n",
				first8(s.SessionID),
				s.SubagentID,
				nullOrDash(s.SubagentType),
				formatTS(s.StartedAtMs),
				formatTS(s.EndedAtMs),
				s.EventCount,
			)
		}
		return TextResult(b.String()), nil
	}
}

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

// --- get_unresolved_for_cwd ---

func getUnresolvedForCwdHandler(st *store.Store) ToolHandler {
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
		// Defaults match the CLI / store defaults so behaviour
		// stays consistent across surfaces.
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

		items, err := store.LoadUnresolvedForCwd(ctx, st.DB(), req.Cwd, sinceMs,
			maxSessions, maxPerSession)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_unresolved_for_cwd: load: " + err.Error()}
		}
		if len(items) == 0 {
			return TextResult(fmt.Sprintf("no unresolved items from prior sessions in %s", req.Cwd)), nil
		}
		// One line per item, same shape as the CLI's text rendering
		// — keeps the agent's reading model consistent across the
		// CLI and MCP surfaces.
		var b strings.Builder
		fmt.Fprintf(&b, "%d unresolved item(s) from prior sessions in %s:\n", len(items), req.Cwd)
		now := time.Now()
		for _, it := range items {
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

// --- get_facts_for_subject ---

func getFactsForSubjectHandler(st *store.Store) ToolHandler {
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
		facts, err := store.LoadFactsForSubject(ctx, st.DB(), req.Subject, req.Limit)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_facts_for_subject: load: " + err.Error()}
		}
		if len(facts) == 0 {
			return TextResult(fmt.Sprintf(
				"(no facts known for %q yet — try `aichronicles facts induce --session <id>` on a past session in this project)",
				req.Subject)), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "subject: %s\n", req.Subject)
		for _, f := range facts {
			fmt.Fprintf(&b, "%s\t%s\t%.2f\t%s\n",
				f.Predicate, f.Object, f.Confidence,
				formatTS(f.AssertedAtMs))
			if f.EvidenceQuote.Valid && f.EvidenceQuote.String != "" {
				fmt.Fprintf(&b, "  quote: %s\n", f.EvidenceQuote.String)
			}
		}
		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

// --- find_fact_subjects ---

func findFactSubjectsHandler(st *store.Store) ToolHandler {
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
		subjects, err := store.FactSubjectsLike(ctx, st.DB(), req.Contains, req.Limit)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "find_fact_subjects: load: " + err.Error()}
		}
		if len(subjects) == 0 {
			return TextResult("(no fact subjects matched)"), nil
		}
		return TextResult(strings.Join(subjects, "\n")), nil
	}
}

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

// nullOrDashEvents is the events.NullString variant of nullOrDash.
// Same render: empty/null → "-", populated → string.
func nullOrDashEvents(s events.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	return s.String
}

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
