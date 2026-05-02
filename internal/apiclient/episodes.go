package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Episodes queries GET /v1/episodes.
func (c *Client) Episodes(ctx context.Context, req api.EpisodeListRequest) (api.EpisodeListResponse, error) {
	q := url.Values{}
	if req.SessionID != "" {
		q.Set("session_id", req.SessionID)
	}
	if req.Cwd != "" {
		q.Set("cwd", req.Cwd)
	}
	if req.QueryContains != "" {
		q.Set("query_contains", req.QueryContains)
	}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/episodes"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.EpisodeListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.EpisodeListResponse{}, err
	}
	return out, nil
}
