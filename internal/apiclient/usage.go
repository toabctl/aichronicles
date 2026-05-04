package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Usage queries GET /v1/usage. Returns one row per (day, kind,
// model) bucket plus the grand totals. sinceMs <= 0 matches every
// llm_outputs row.
func (c *Client) Usage(ctx context.Context, req api.UsageRequest) (api.UsageResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	path := "/v1/usage"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.UsageResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.UsageResponse{}, err
	}
	return out, nil
}
