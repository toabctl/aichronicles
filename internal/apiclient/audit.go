package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Audit queries GET /v1/audit. The server runs the canonical
// redact pattern set against every event with non-null
// content_text and returns one finding per matched event plus
// aggregate counters. Snippet bytes never carry the raw secret —
// the matched span is rendered as <pattern> on the wire.
func (c *Client) Audit(ctx context.Context, req wire.AuditRequest) (wire.AuditResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt("limit", req.Limit)
	var out wire.AuditResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/audit"), nil, &out); err != nil {
		return wire.AuditResponse{}, err
	}
	return out, nil
}
