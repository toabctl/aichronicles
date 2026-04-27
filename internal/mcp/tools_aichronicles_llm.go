package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// summaryTopNDefault and summaryTopNMax cap how many hits feed the
// LLM grounding context for one MCP search_with_summary call. Same
// numbers the CLI uses by default; agents that want a tighter
// answer pass `top_n` explicitly.
const (
	summaryTopNDefault = 5
	summaryTopNMax     = 10
	summaryMaxTokens   = 512
)

// RegisterAichroniclesLLMTools wires up tools that need an
// llm.Client. Kept separate from RegisterAichroniclesTools so
// callers without an API key (CI, read-only setups) can still run
// MCP — the LLM-backed tools just don't appear in the tool list.
//
// newClient is called per request, lazily; the LLM client is not
// constructed at server startup. This matches the pattern in
// internal/cli where the API key is only required when an LLM
// path actually runs.
func RegisterAichroniclesLLMTools(s *Server, st *store.Store, newClient func() (llm.Client, error)) {
	s.RegisterTool(Tool{
		Name: "search_with_summary",
		Description: "Like search_events, but returns ONE LLM-synthesised answer grounded in the top-N hits " +
			"instead of a raw row list. " +
			"Use when the user asks an open-ended question about past work — " +
			"'what was the conclusion of the auth investigation?', 'summarize last month's debugging', " +
			"'remind me how I fixed X' — and expects a digested response, not a list to scan. " +
			"Each claim in the answer is cited by 8-char session_id so the user can drill into the source. " +
			"Costs one LLM call per invocation; prefer plain search_events for cheap keyword lookup.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":  {"type": "string"},
				"top_n":  {"type": "integer", "minimum": 1, "maximum": 10, "default": 5,
				           "description": "Max hits fed to the LLM as grounding context."},
				"kind":   {"type": "string", "description": "Optional event-kind filter (user_prompt, tool_use, …)."},
				"since_hours": {"type": "integer", "minimum": 1,
				                "description": "Optional time window: only events within this many hours."}
			},
			"required": ["query"]
		}`),
		Handler: searchWithSummaryHandler(st, newClient),
	})
}

func searchWithSummaryHandler(st *store.Store, newClient func() (llm.Client, error)) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Query      string `json:"query"`
			TopN       int    `json:"top_n"`
			Kind       string `json:"kind"`
			SinceHours int    `json:"since_hours"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "search_with_summary: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Query) == "" {
			return TextError("search_with_summary: query is required"), nil
		}

		topN := req.TopN
		if topN <= 0 {
			topN = summaryTopNDefault
		}
		if topN > summaryTopNMax {
			topN = summaryTopNMax
		}

		ftsQuery, err := searchquery.ToFTS5(req.Query)
		if err != nil {
			switch {
			case errors.Is(err, searchquery.ErrEmpty):
				return TextError("search_with_summary: query is required"), nil
			case errors.Is(err, searchquery.ErrSyntax):
				return TextError("search_with_summary: %v", err), nil
			default:
				return nil, &Error{Code: InvalidParams, Message: "search_with_summary: parse query: " + err.Error()}
			}
		}

		opts := store.SearchEventOpts{
			Query: ftsQuery,
			Kind:  req.Kind,
			Limit: topN,
			Order: store.OrderRank,
		}
		if req.SinceHours > 0 {
			opts.SinceMs = nowMs() - int64(req.SinceHours)*60*60*1000
		}
		hits, err := store.SearchEvents(ctx, st.DB(), opts)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "search_with_summary: query: " + err.Error()}
		}
		if len(hits) == 0 {
			return TextResult("(no hits)"), nil
		}

		promptHits := make([]prompts.SearchHit, 0, len(hits))
		for _, h := range hits {
			cwd := ""
			if h.Cwd.Valid {
				cwd = h.Cwd.String
			}
			snip := ""
			if h.Content.Valid {
				snip = h.Content.String
			}
			promptHits = append(promptHits, prompts.SearchHit{
				SessionID:  h.SessionID,
				Kind:       h.Kind,
				Cwd:        cwd,
				TsSourceMs: h.TsSourceMs,
				Snippet:    snip,
			})
		}

		built, err := prompts.BuildSearchSummary(req.Query, promptHits, summaryMaxTokens)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "search_with_summary: build prompt: " + err.Error()}
		}

		client, err := newClient()
		if err != nil {
			return TextError("search_with_summary: LLM client unavailable: %v", err), nil
		}
		resp, err := client.Complete(ctx, built.Request)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "search_with_summary: LLM call: " + err.Error()}
		}

		// Tail the answer with a compact citations block: short
		// session_ids the agent can pass back to search_events
		// (subagent_id stays unused; this is a session-scoped
		// drilldown). Keeps the model's prose tight while still
		// surfacing the hit set deterministically.
		var b strings.Builder
		b.WriteString(strings.TrimRight(resp.Text, "\n"))
		b.WriteString("\n\nGrounded in:\n")
		for _, h := range hits {
			id := h.SessionID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(&b, "  [%s] %s\n", id, h.Kind)
		}
		return TextResult(b.String()), nil
	}
}

// nowMs is split out so tests can override clock dependence if
// needed; today it's a thin wrapper over time.Now().
var nowMs = func() int64 {
	return time.Now().UTC().UnixMilli()
}
