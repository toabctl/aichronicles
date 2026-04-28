package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/store"
)

// RegisterAichroniclesTools adds the three read-only tools Block C
// ships (search_events, list_sessions, get_summary). Callers that
// want a different set can register their own without touching this.
//
// All tools read through *store.Store — no privileged writes. An
// MCP client that compromises its sandbox still only reads a subset
// of already-stored events, already scrubbed at ingest.
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
			(SELECT content_text FROM events
				WHERE session_id = s.id AND kind = 'user_prompt'
				ORDER BY ts_source_ms ASC LIMIT 1) AS first_prompt
			FROM sessions s WHERE 1=1` + filter.String() + `
			ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
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

// relativeAgo formats epoch-millis as a short relative time. Local
// to this package so the MCP tools don't reach into internal/cli
// for its formatter — the layering is one-way (cli → mcp would
// induce a cycle, and the formatter is small enough to duplicate).
func relativeAgo(ms int64, now time.Time) string {
	if ms <= 0 {
		return "active"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 0:
		return "future?"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return time.UnixMilli(ms).UTC().Format("2006-01-02")
	}
}

// --- helpers ---

func first8(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

func formatTS(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

func formatTSNullable(n sql.NullInt64) string {
	if !n.Valid {
		return "-"
	}
	return formatTS(n.Int64)
}

func nullOrDash(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	return s.String
}

// oneLineSnippet flattens whitespace and caps a content preview so
// each tool row stays on a single terminal line.
func oneLineSnippet(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "-"
	}
	text := s.String
	for _, r := range "\n\r\t" {
		text = strings.ReplaceAll(text, string(r), " ")
	}
	const maxRunes = 120
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}
