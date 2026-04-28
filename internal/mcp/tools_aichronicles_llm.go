package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
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

// semanticSearchDefaultLimit / semanticSearchMaxLimit cap the result
// set for the MCP semantic_search_events tool. Same shape as
// search_events for predictability across the keyword and semantic
// surfaces.
const (
	semanticSearchDefaultLimit = 20
	semanticSearchMaxLimit     = 50
)

// RegisterAichroniclesEmbeddingTools wires up tools that need an
// llm.Embedder. Kept separate from RegisterAichroniclesLLMTools so
// chat-only callers (no embedding model configured) don't see the
// semantic surface advertised when it can't actually work.
//
// newEmbedder is called per request, lazily — same pattern as the
// chat-LLM constructor. Construction failure surfaces as a tool-use
// error rather than a startup error so the rest of the MCP server
// stays available.
func RegisterAichroniclesEmbeddingTools(s *Server, st *store.Store, newEmbedder func() (llm.Embedder, error)) {
	s.RegisterTool(Tool{
		Name: "semantic_search_events",
		Description: "Search the user's PAST sessions by SEMANTIC similarity, not keyword. " +
			"The query is embedded by the configured provider; stored event embeddings are " +
			"reranked by cosine similarity. Returns hits with session id, kind, timestamp, " +
			"and a snippet of content_text. " +
			"Use when the user asks about a CONCEPT or topic without naming the exact word " +
			"that would appear in the source — e.g. 'find sessions about authentication issues' " +
			"when the actual transcripts say 'login bug', 'session token rejected', etc. " +
			"Plain keyword search (search_events) is faster and cheaper; reach for semantic " +
			"search when keyword recall is failing. " +
			"Requires events to have been embedded already via `aichronicles embed`. " +
			"Returns (no hits) if no embeddings exist or none match the filters.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":        {"type": "string", "description": "Free-text query embedded into the same vector space as stored events."},
				"session_id":   {"type": "string", "description": "Narrow to one session (full UUID or unique prefix)."},
				"kind":         {"type": "string", "description": "Optional event-kind filter (user_prompt, tool_use, …)."},
				"since_hours":  {"type": "integer", "minimum": 1, "description": "Optional time window: only events within this many hours."},
				"limit":        {"type": "integer", "minimum": 1, "maximum": 50, "default": 20}
			},
			"required": ["query"]
		}`),
		Handler: semanticSearchHandler(st, newEmbedder),
	})

	s.RegisterTool(Tool{
		Name: "find_workflows",
		Description: "Find workflows whose task_shape is semantically similar to a free-text " +
			"query. Embeds the query and every workflow's task_shape, ranks by cosine similarity, " +
			"returns the top-K. Use when the user is about to start a task and wants to know " +
			"whether a similar shape has been done before — e.g. 'I need to roll out a config " +
			"change to staging' should retrieve a 'deploy a backend service to staging' workflow " +
			"even if the wording differs. " +
			"Distinct from list_workflows (substring match on task_shape, no LLM cost): semantic " +
			"ranking is more recall-friendly but costs one embedding call per request. " +
			"Returns (no workflows yet) when the workflow corpus is empty.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Free-text task description. Embedded into the same vector space as the workflow task_shapes."},
				"limit": {"type": "integer", "minimum": 1, "maximum": 20, "default": 5}
			},
			"required": ["query"]
		}`),
		Handler: findWorkflowsHandler(st, newEmbedder),
	})
}

func semanticSearchHandler(st *store.Store, newEmbedder func() (llm.Embedder, error)) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Query      string `json:"query"`
			SessionID  string `json:"session_id"`
			Kind       string `json:"kind"`
			SinceHours int    `json:"since_hours"`
			Limit      int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "semantic_search_events: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Query) == "" {
			return TextError("semantic_search_events: query is required"), nil
		}
		if req.Limit <= 0 {
			req.Limit = semanticSearchDefaultLimit
		}
		if req.Limit > semanticSearchMaxLimit {
			req.Limit = semanticSearchMaxLimit
		}

		// Resolve session prefix to its full UUID before passing to
		// SemanticSearch — match the search_events surface, which
		// also accepts prefixes (via the agent's typical 8-char
		// short-id workflow).
		if req.SessionID != "" {
			full, err := store.ResolveSessionIDPrefix(ctx, st.DB(), req.SessionID)
			if err != nil {
				return TextError("semantic_search_events: %v", err), nil
			}
			req.SessionID = full
		}

		embedder, err := newEmbedder()
		if err != nil {
			return TextError("semantic_search_events: embedder unavailable: %v", err), nil
		}
		model := llm.DefaultEmbeddingModel
		resp, err := embedder.Embed(ctx, llm.EmbedRequest{
			Model:  model,
			Inputs: []string{req.Query},
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "semantic_search_events: embed query: " + err.Error()}
		}
		if len(resp.Vectors) != 1 {
			return nil, &Error{Code: InternalError, Message: fmt.Sprintf(
				"semantic_search_events: expected 1 query vector, got %d", len(resp.Vectors))}
		}
		queryVec := resp.Vectors[0]

		opts := store.SemanticSearchOpts{
			QueryVec:  queryVec,
			Model:     model,
			Dim:       len(queryVec),
			SessionID: req.SessionID,
			Kind:      req.Kind,
			TopK:      req.Limit,
		}
		if req.SinceHours > 0 {
			opts.SinceMs = nowMs() - int64(req.SinceHours)*60*60*1000
		}
		hits, err := store.SemanticSearch(ctx, st.DB(), opts)
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "semantic_search_events: search: " + err.Error()}
		}
		if len(hits) == 0 {
			return TextResult("(no hits — try `aichronicles embed` if embeddings haven't been backfilled, or relax filters)"), nil
		}

		var b strings.Builder
		for _, h := range hits {
			snip := ""
			if h.Content.Valid {
				snip = previewSemanticSnippet(h.Content.String)
			}
			fmt.Fprintf(&b, "%s\t%s\t%.3f\t%s\t%s\n",
				first8(h.SessionID),
				time.UnixMilli(h.TsSourceMs).UTC().Format(time.RFC3339),
				h.Score,
				h.Kind,
				snip,
			)
		}
		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

// findWorkflowsCorpusCap is the maximum number of induction rows
// we'll scan per find_workflows call. Workflows are sparse (most
// induction rows have body.workflow=null), so this loosely
// translates to "the most recent ~N workflow inductions" rather
// than 200 actual workflows. At our scale a brute-force embed +
// cosine over the whole corpus is fine; if the corpus grows
// past a few thousand workflows, we'd add a workflow_embeddings
// cache here.
const findWorkflowsCorpusCap = 200

func findWorkflowsHandler(st *store.Store, newEmbedder func() (llm.Embedder, error)) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*ToolResult, *Error) {
		var req struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, &Error{Code: InvalidParams, Message: "find_workflows: bad args: " + err.Error()}
		}
		if strings.TrimSpace(req.Query) == "" {
			return TextError("find_workflows: query is required"), nil
		}
		if req.Limit <= 0 || req.Limit > 20 {
			req.Limit = 5
		}

		// Pull the workflow corpus from kind=induction rows where
		// body.workflow is non-null. Round 8 collapsed kind=workflow
		// into the unified induction body.
		rows, err := store.LoadLLMOutputs(ctx, st.DB(), store.LLMOutputFilter{
			Kind:  store.LLMKindInduction,
			Limit: findWorkflowsCorpusCap,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "find_workflows: load: " + err.Error()}
		}
		type candidate struct {
			row store.LLMOutput
			ind prompts.InductionResult
		}
		var corpus []candidate
		for _, r := range rows {
			var ind prompts.InductionResult
			if jerr := json.Unmarshal([]byte(r.Body), &ind); jerr != nil {
				continue
			}
			if ind.Workflow == nil || strings.TrimSpace(ind.Workflow.TaskShape) == "" {
				continue
			}
			corpus = append(corpus, candidate{row: r, ind: ind})
		}
		if len(corpus) == 0 {
			return TextResult("(no workflows yet — workflows are emitted by `aichronicles induction sweep` alongside skills)"), nil
		}

		// One batched embed call: [query, taskShape0, taskShape1, ...].
		// Response.Vectors[0] is the query, [1:] correspond 1:1 with
		// corpus entries.
		embedder, err := newEmbedder()
		if err != nil {
			return TextError("find_workflows: embedder unavailable: %v", err), nil
		}
		inputs := make([]string, 0, 1+len(corpus))
		inputs = append(inputs, req.Query)
		for _, c := range corpus {
			inputs = append(inputs, c.ind.Workflow.TaskShape)
		}
		resp, err := embedder.Embed(ctx, llm.EmbedRequest{
			Model:  llm.DefaultEmbeddingModel,
			Inputs: inputs,
		})
		if err != nil {
			return nil, &Error{Code: InternalError, Message: "find_workflows: embed: " + err.Error()}
		}
		if len(resp.Vectors) != len(inputs) {
			return nil, &Error{Code: InternalError, Message: fmt.Sprintf(
				"find_workflows: expected %d vectors, got %d", len(inputs), len(resp.Vectors))}
		}

		queryVec := resp.Vectors[0]
		shapeVecs := resp.Vectors[1:]

		type scored struct {
			cand  candidate
			score float64
		}
		ranked := make([]scored, 0, len(corpus))
		for i, c := range corpus {
			ranked = append(ranked, scored{cand: c, score: float64(cosine(queryVec, shapeVecs[i]))})
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
		if len(ranked) > req.Limit {
			ranked = ranked[:req.Limit]
		}

		var b strings.Builder
		for _, r := range ranked {
			sessShort := "(none)"
			if r.cand.row.SessionID.Valid && len(r.cand.row.SessionID.String) >= 8 {
				sessShort = r.cand.row.SessionID.String[:8]
			}
			fmt.Fprintf(&b, "%s\t%.3f\t%s\n",
				sessShort, r.score, r.cand.ind.Workflow.TaskShape)
			for i, step := range r.cand.ind.Workflow.Procedure {
				fmt.Fprintf(&b, "  %d. %s\n", i+1, step.Action)
			}
		}
		return TextResult(strings.TrimRight(b.String(), "\n")), nil
	}
}

// cosine computes cosine similarity between two equal-length
// float32 vectors. Returns 0 on length mismatch or zero norm — same
// degenerate-but-finite contract the store package's cosineNormed
// uses; here we keep a local copy because the find_workflows
// handler doesn't need the precomputed-norm optimisation
// SemanticSearch wants for its inner-loop hot path.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(na*nb))
}

// previewSemanticSnippet collapses whitespace and rune-truncates the
// content for the tabular result row so a long event body doesn't
// overflow the agent's display.
func previewSemanticSnippet(s string) string {
	const maxRunes = 160
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return string(r)
}
