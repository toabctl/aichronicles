package apiclient

import (
	"context"
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
	q := url.Values{}
	q.Set("kind", kind)
	var out wire.LLMOutputLastCreatedAtResponse
	if err := c.do(ctx, http.MethodGet, "/v1/llm-outputs/last-created-at?"+q.Encode(), nil, &out); err != nil {
		return 0, err
	}
	return out.LastCreatedAtMs, nil
}

// LLMOutputExistsForSession probes whether a kind-row already
// exists for the named session. Used by the induction sweeper to
// short-circuit phase 1 (auto-summarize) when the row is already
// there.
func (c *Client) LLMOutputExistsForSession(ctx context.Context, sessionID, kind string) (bool, error) {
	q := url.Values{}
	q.Set("session_id", sessionID)
	q.Set("kind", kind)
	var out wire.LLMOutputExistsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/llm-outputs/exists?"+q.Encode(), nil, &out); err != nil {
		return false, err
	}
	return out.Exists, nil
}

// SessionLLMOutputs fetches every llm_outputs row for a session,
// optionally filtered by kind. Used by MCP get_summary (when
// kind != summary).
func (c *Client) SessionLLMOutputs(ctx context.Context, sessionID, kind string, limit int) ([]wire.LLMOutput, error) {
	q := url.Values{}
	if kind != "" {
		q.Set("kind", kind)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/llm-outputs"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.LLMOutputsListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Outputs, nil
}

// LLMOutputsList fetches a filtered list of LLM outputs across
// sessions. Used by MCP list_workflows (kind=induction).
func (c *Client) LLMOutputsList(ctx context.Context, kind, sessionID string, limit int) ([]wire.LLMOutput, error) {
	q := url.Values{}
	if kind != "" {
		q.Set("kind", kind)
	}
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/llm-outputs/list"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.LLMOutputsListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Outputs, nil
}

// Summary fetches the cached summary for a session, or
// ErrNotFound when none exists.
func (c *Client) Summary(ctx context.Context, sessionID string) (wire.LLMOutput, error) {
	q := url.Values{}
	q.Set("session_id", sessionID)
	var out wire.LLMOutput
	if err := c.do(ctx, http.MethodGet, "/v1/summaries?"+q.Encode(), nil, &out); err != nil {
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
	q := url.Values{}
	q.Set("session_ids", strings.Join(ids, ","))
	var out wire.SummariesBatchResponse
	if err := c.do(ctx, http.MethodGet, "/v1/summaries/batch?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Summaries, nil
}

// LLMOutputByHash fetches the cached llm output for a (kind,
// prompt_hash) pair. Returns ErrNotFound when missing.
func (c *Client) LLMOutputByHash(ctx context.Context, kind, promptHash string) (wire.LLMOutput, error) {
	q := url.Values{}
	q.Set("kind", kind)
	q.Set("prompt_hash", promptHash)
	var out wire.LLMOutput
	if err := c.do(ctx, http.MethodGet, "/v1/llm-outputs?"+q.Encode(), nil, &out); err != nil {
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
	q := url.Values{}
	q.Set("cwd", req.Cwd)
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.MaxSessions > 0 {
		q.Set("max_sessions", strconv.Itoa(req.MaxSessions))
	}
	if req.MaxItemsPerSession > 0 {
		q.Set("max_items_per_session", strconv.Itoa(req.MaxItemsPerSession))
	}
	var out wire.UnresolvedResponse
	if err := c.do(ctx, http.MethodGet, "/v1/unresolved?"+q.Encode(), nil, &out); err != nil {
		return wire.UnresolvedResponse{}, err
	}
	return out, nil
}

// ProjectAggregates fetches /v1/projects/aggregates.
func (c *Client) ProjectAggregates(ctx context.Context, sinceMs int64) (wire.ProjectAggregatesResponse, error) {
	path := "/v1/projects/aggregates"
	if sinceMs > 0 {
		path += "?since_ms=" + strconv.FormatInt(sinceMs, 10)
	}
	var out wire.ProjectAggregatesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.ProjectAggregatesResponse{}, err
	}
	return out, nil
}

// SubagentSpans fetches /v1/subagents.
func (c *Client) SubagentSpans(ctx context.Context, sessionID string, limit int) (wire.SubagentsResponse, error) {
	q := url.Values{}
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/subagents"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.SubagentsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
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
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.TopTools > 0 {
		q.Set("top_tools", strconv.Itoa(req.TopTools))
	}
	if req.TopSkills > 0 {
		q.Set("top_skills", strconv.Itoa(req.TopSkills))
	}
	if req.TopSessions > 0 {
		q.Set("top_sessions", strconv.Itoa(req.TopSessions))
	}
	path := "/v1/insights"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.Insights
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.Insights{}, err
	}
	return out, nil
}
