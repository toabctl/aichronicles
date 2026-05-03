package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Search queries GET /v1/search. Q is required; the server
// returns 400 when it is empty.
func (c *Client) Search(ctx context.Context, req api.SearchRequest) (api.SearchResponse, error) {
	q := url.Values{}
	q.Set("q", req.Q)
	for k, v := range map[string]string{
		"kind":                req.Kind,
		"session_id":          req.SessionID,
		"subagent_id":         req.SubagentID,
		"source_agent":        req.SourceAgent,
		"tool_name":           req.ToolName,
		"skill_name":          req.SkillName,
		"file_path_substring": req.FilePathSubstring,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if req.WithFailures {
		q.Set("with_failures", "true")
	}
	if req.NoDedup {
		q.Set("no_dedup", "true")
	}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}

	var out api.SearchResponse
	if err := c.do(ctx, http.MethodGet, "/v1/search?"+q.Encode(), nil, &out); err != nil {
		return api.SearchResponse{}, err
	}
	return out, nil
}
