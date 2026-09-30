package apiclient

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/toabctl/aichronicles/internal/wire"
)

// LLMOutputByID fetches one llm_outputs row by primary key.
// ErrNotFound when the row does not exist.
func (c *Client) LLMOutputByID(ctx context.Context, id int64) (wire.LLMOutput, error) {
	var out wire.LLMOutput
	if err := c.do(ctx, http.MethodGet, "/v1/llm-outputs/"+strconv.FormatInt(id, 10), nil, &out); err != nil {
		return wire.LLMOutput{}, err
	}
	return out, nil
}

// LLMOutputLastCreatedAt queries
// GET /v1/llm-outputs/last-created-at?kind=. Returns the most-recent
// created_at_ms for a given kind, or 0 when no rows match. Drives
// the meta sweeper's per-kind cadence gate.
func (c *Client) LLMOutputLastCreatedAt(ctx context.Context, kind string) (int64, error) {
	var q qparams
	q.SetString("kind", kind)
	var out wire.LLMOutputLastCreatedAtResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/llm-outputs/last-created-at"), nil, &out); err != nil {
		return 0, err
	}
	return out.LastCreatedAtMs, nil
}

// LLMOutputExistsForSession probes whether a kind-row already
// exists for the named session. Used by the induction sweeper to
// short-circuit phase 1 (auto-summarize) when the row is already
// there.
func (c *Client) LLMOutputExistsForSession(ctx context.Context, sessionID, kind string) (bool, error) {
	var q qparams
	q.SetString("session_id", sessionID)
	q.SetString("kind", kind)
	var out wire.LLMOutputExistsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/llm-outputs/exists"), nil, &out); err != nil {
		return false, err
	}
	return out.Exists, nil
}

// SessionLLMOutputs fetches the first page (newest first, at most
// limit rows; 0 = the server default) of a session's llm_outputs,
// optionally filtered by kind. Used by MCP get_summary and the
// summaries CLI, which ask for the single newest row of one kind.
func (c *Client) SessionLLMOutputs(ctx context.Context, sessionID, kind string, limit int) ([]wire.LLMOutput, error) {
	var q qparams
	q.SetString("kind", kind)
	q.SetInt("limit", limit)
	var out wire.LLMOutputsListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/"+url.PathEscape(sessionID)+"/llm-outputs"), nil, &out); err != nil {
		return nil, err
	}
	return out.Outputs, nil
}

// LLMOutputsList fetches the first page (newest first, at most limit
// rows; 0 = the server default) of LLM outputs across sessions,
// filtered by kind and/or session. For "every matching row" use
// LLMOutputs, which follows the cursor.
func (c *Client) LLMOutputsList(ctx context.Context, kind, sessionID string, limit int) ([]wire.LLMOutput, error) {
	resp, err := c.llmOutputsPage(ctx, kind, sessionID, limit, "")
	if err != nil {
		return nil, err
	}
	return resp.Outputs, nil
}

func (c *Client) llmOutputsPage(ctx context.Context, kind, sessionID string, limit int, cursor wire.Cursor) (wire.LLMOutputsListResponse, error) {
	var q qparams
	q.SetString("kind", kind)
	q.SetString("session_id", sessionID)
	q.SetInt("limit", limit)
	q.SetString("cursor", string(cursor))
	var out wire.LLMOutputsListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/llm-outputs"), nil, &out); err != nil {
		return wire.LLMOutputsListResponse{}, err
	}
	return out, nil
}

// LLMOutputs yields every LLM output matching kind and/or session,
// newest first, fetching pages lazily by following next_cursor. Stop
// early by breaking out of the range loop; a fetch error is yielded
// once and ends the sequence. Use it wherever a consumer filters rows
// client-side (e.g. induction rows that carry a workflow): filtering
// one page and reporting "none" when the match sits on page two is
// the bug it replaces.
//
// The endpoint's offset pagination is bounded at wire.MaxOffset rows;
// a walk that reaches it ends with that 400 as its error.
func (c *Client) LLMOutputs(ctx context.Context, kind, sessionID string) iter.Seq2[wire.LLMOutput, error] {
	return func(yield func(wire.LLMOutput, error) bool) {
		var cursor wire.Cursor
		for {
			page, err := c.llmOutputsPage(ctx, kind, sessionID, wire.MaxPageLimit, cursor)
			if err != nil {
				yield(wire.LLMOutput{}, err)
				return
			}
			for _, o := range page.Outputs {
				if !yield(o, nil) {
					return
				}
			}
			if page.NextCursor == "" {
				return
			}
			if page.NextCursor == cursor || len(page.Outputs) == 0 {
				yield(wire.LLMOutput{}, fmt.Errorf("apiclient: llm-outputs cursor did not advance"))
				return
			}
			cursor = page.NextCursor
		}
	}
}

// Summary fetches the cached summary for a session, or
// ErrNotFound when none exists.
func (c *Client) Summary(ctx context.Context, sessionID string) (wire.LLMOutput, error) {
	var q qparams
	q.SetString("session_id", sessionID)
	var out wire.LLMOutput
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/summaries"), nil, &out); err != nil {
		return wire.LLMOutput{}, err
	}
	return out, nil
}

// SummariesBatch fetches latest summaries for many sessions in one
// round-trip. Returns a map keyed by session_id; sessions without a
// cached summary are absent (not nil-valued). An empty ids slice
// returns an empty map and a 400 — the caller should skip the call
// when it has nothing to fetch.
func (c *Client) SummariesBatch(ctx context.Context, ids []string) (map[string]wire.LLMOutput, error) {
	if len(ids) == 0 {
		return map[string]wire.LLMOutput{}, nil
	}
	var q qparams
	q.SetString("session_ids", strings.Join(ids, ","))
	var out wire.SummariesBatchResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/summaries/batch"), nil, &out); err != nil {
		return nil, err
	}
	return out.Summaries, nil
}

// LLMOutputByHash fetches the cached llm output for a (kind,
// prompt_hash) pair. Returns ErrNotFound when missing.
func (c *Client) LLMOutputByHash(ctx context.Context, kind, promptHash string) (wire.LLMOutput, error) {
	var q qparams
	q.SetString("kind", kind)
	q.SetString("prompt_hash", promptHash)
	var out wire.LLMOutput
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/llm-outputs/by-hash"), nil, &out); err != nil {
		return wire.LLMOutput{}, err
	}
	return out, nil
}

// UnresolvedRequest is the query-shape for /v1/unresolved.
// Cwd is required.
type UnresolvedRequest struct {
	Cwd                string
	SinceMs            int64
	MaxSessions        int
	MaxItemsPerSession int
}

func (c *Client) Unresolved(ctx context.Context, req UnresolvedRequest) (wire.UnresolvedResponse, error) {
	var q qparams
	q.SetString("cwd", req.Cwd)
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt("max_sessions", req.MaxSessions)
	q.SetInt("max_items_per_session", req.MaxItemsPerSession)
	var out wire.UnresolvedResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/unresolved"), nil, &out); err != nil {
		return wire.UnresolvedResponse{}, err
	}
	return out, nil
}

// ProjectAggregates fetches /v1/projects/aggregates.
func (c *Client) ProjectAggregates(ctx context.Context, sinceMs int64) (wire.ProjectAggregatesResponse, error) {
	var q qparams
	q.SetInt64("since_ms", sinceMs)
	var out wire.ProjectAggregatesResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/projects/aggregates"), nil, &out); err != nil {
		return wire.ProjectAggregatesResponse{}, err
	}
	return out, nil
}

// SubagentSpans fetches /v1/subagents.
func (c *Client) SubagentSpans(ctx context.Context, sessionID string, limit int) (wire.SubagentsResponse, error) {
	var q qparams
	q.SetString("session_id", sessionID)
	q.SetInt("limit", limit)
	var out wire.SubagentsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/subagents"), nil, &out); err != nil {
		return wire.SubagentsResponse{}, err
	}
	return out, nil
}

// InsightsRequest is the query-shape for /v1/insights.
type InsightsRequest struct {
	SinceMs     int64
	TopTools    int
	TopSkills   int
	TopSessions int
}

func (c *Client) Insights(ctx context.Context, req InsightsRequest) (wire.Insights, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt("top_tools", req.TopTools)
	q.SetInt("top_skills", req.TopSkills)
	q.SetInt("top_sessions", req.TopSessions)
	var out wire.Insights
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/insights"), nil, &out); err != nil {
		return wire.Insights{}, err
	}
	return out, nil
}
