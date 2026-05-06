package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Episodes queries GET /v1/episodes.
func (c *Client) Episodes(ctx context.Context, req wire.EpisodeListRequest) (wire.EpisodeListResponse, error) {
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
	var out wire.EpisodeListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.EpisodeListResponse{}, err
	}
	return out, nil
}
