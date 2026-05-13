package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Usage queries GET /v1/usage. Returns one row per (day, kind,
// model) bucket plus the grand totals. sinceMs <= 0 matches every
// llm_outputs row.
func (c *Client) Usage(ctx context.Context, req wire.UsageRequest) (wire.UsageResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	var out wire.UsageResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/usage"), nil, &out); err != nil {
		return wire.UsageResponse{}, err
	}
	return out, nil
}
