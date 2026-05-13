package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Episodes queries GET /v1/episodes.
func (c *Client) Episodes(ctx context.Context, req wire.EpisodeListRequest) (wire.EpisodeListResponse, error) {
	var q qparams
	q.SetString("session_id", req.SessionID)
	q.SetString("cwd", req.Cwd)
	q.SetString("query_contains", req.QueryContains)
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt("limit", req.Limit)
	var out wire.EpisodeListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/episodes"), nil, &out); err != nil {
		return wire.EpisodeListResponse{}, err
	}
	return out, nil
}
