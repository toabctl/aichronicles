package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Summary fetches the cached summary for a session, or
// ErrNotFound when none exists.
func (c *Client) Summary(ctx context.Context, sessionID string) (api.LLMOutput, error) {
	q := url.Values{}
	q.Set("session_id", sessionID)
	var out api.LLMOutput
	if err := c.do(ctx, http.MethodGet, "/v1/summaries?"+q.Encode(), nil, &out); err != nil {
		return api.LLMOutput{}, err
	}
	return out, nil
}

// LLMOutputByHash fetches the cached llm output for a (kind,
// prompt_hash) pair. Returns ErrNotFound when missing.
func (c *Client) LLMOutputByHash(ctx context.Context, kind, promptHash string) (api.LLMOutput, error) {
	q := url.Values{}
	q.Set("kind", kind)
	q.Set("prompt_hash", promptHash)
	var out api.LLMOutput
	if err := c.do(ctx, http.MethodGet, "/v1/llm-outputs?"+q.Encode(), nil, &out); err != nil {
		return api.LLMOutput{}, err
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

func (c *Client) Unresolved(ctx context.Context, req UnresolvedRequest) (api.UnresolvedResponse, error) {
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
	var out api.UnresolvedResponse
	if err := c.do(ctx, http.MethodGet, "/v1/unresolved?"+q.Encode(), nil, &out); err != nil {
		return api.UnresolvedResponse{}, err
	}
	return out, nil
}

// ProjectAggregates fetches /v1/projects/aggregates.
func (c *Client) ProjectAggregates(ctx context.Context, sinceMs int64) (api.ProjectAggregatesResponse, error) {
	path := "/v1/projects/aggregates"
	if sinceMs > 0 {
		path += "?since_ms=" + strconv.FormatInt(sinceMs, 10)
	}
	var out api.ProjectAggregatesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.ProjectAggregatesResponse{}, err
	}
	return out, nil
}

// SubagentSpans fetches /v1/subagents.
func (c *Client) SubagentSpans(ctx context.Context, sessionID string, limit int) (api.SubagentsResponse, error) {
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
	var out api.SubagentsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SubagentsResponse{}, err
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

func (c *Client) Insights(ctx context.Context, req InsightsRequest) (api.Insights, error) {
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
	var out api.Insights
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.Insights{}, err
	}
	return out, nil
}
