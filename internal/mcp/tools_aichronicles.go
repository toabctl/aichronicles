package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
		Name:        "search_events",
		Description: "Full-text search over aichronicles events. Returns the top-matching events with session id, kind, and content snippet.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "SQLite FTS5 MATCH query"},
				"limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}
			},
			"required": ["query"]
		}`),
		Handler: searchEventsHandler(st),
	})

	s.RegisterTool(Tool{
		Name:        "list_sessions",
		Description: "List recent aichronicles sessions, newest first. Optional filters for working directory and time window.",
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
		Name:        "get_summary",
		Description: "Fetch the latest LLM-generated output for a session (kind=summary by default; accepts reflect/propose).",
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
}

// --- search_events ---

func searchEventsHandler(st *store.Store) ToolHandler {
	return func(_ context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
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

		// Reuse the deduped SQL shape so tools/search and CLI/search
		// return consistent rows. Duplicating the SQL here keeps the
		// mcp package free of a cli/* import cycle.
		rows, err := st.DB().Query(`
			WITH matched AS (
				SELECT e.session_id, e.kind, e.role, e.cwd, e.ts_source_ms, e.content_text,
					ROW_NUMBER() OVER (
						PARTITION BY e.session_id, e.role, e.kind, COALESCE(e.content_text, e.rowid)
						ORDER BY e.rowid
					) AS rn
				FROM events_fts
				JOIN events e ON e.rowid = events_fts.rowid
				WHERE events_fts MATCH ?
			)
			SELECT session_id, kind, ts_source_ms, content_text
			FROM matched
			WHERE rn = 1
			ORDER BY ts_source_ms DESC
			LIMIT ?`, req.Query, req.Limit)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "search_events: query: " + err.Error()}
		}
		defer func() { _ = rows.Close() }()

		var b strings.Builder
		hits := 0
		for rows.Next() {
			var sessID, kind string
			var tsMs int64
			var content sql.NullString
			if err := rows.Scan(&sessID, &kind, &tsMs, &content); err != nil {
				return nil, &Error{Code: InternalError, Message: "search_events: scan: " + err.Error()}
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n",
				first8(sessID),
				formatTS(tsMs),
				kind,
				oneLineSnippet(content),
			)
			hits++
		}
		if err := rows.Err(); err != nil {
			return nil, &Error{Code: InternalError, Message: "search_events: rows: " + err.Error()}
		}
		if hits == 0 {
			return TextResult("(no hits)"), nil
		}
		return TextResult(b.String()), nil
	}
}

// --- list_sessions ---

func listSessionsHandler(st *store.Store) ToolHandler {
	return func(_ context.Context, args json.RawMessage) (*ToolResult, *Error) {
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

		rows, err := st.DB().Query(q, sqlArgs...)
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

// --- get_summary ---

func getSummaryHandler(st *store.Store) ToolHandler {
	return func(_ context.Context, args json.RawMessage) (*ToolResult, *Error) {
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

		outs, err := store.LoadLLMOutputsForSession(st.DB(), req.SessionID)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "get_summary: load: " + err.Error()}
		}
		for _, o := range outs {
			if o.Kind == kind {
				return TextResult(o.Body), nil
			}
		}
		return TextError("no %s output for session %s", kind, req.SessionID), nil
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
