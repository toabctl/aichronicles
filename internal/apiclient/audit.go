package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Audit queries GET /v1/audit. The server runs the canonical
// redact pattern set against every event with non-null
// content_text and returns one finding per matched event plus
// aggregate counters. Snippet bytes never carry the raw secret —
// the matched span is rendered as <pattern> on the wire.
func (c *Client) Audit(ctx context.Context, req wire.AuditRequest) (wire.AuditResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/audit"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.AuditResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.AuditResponse{}, err
	}
	return out, nil
}
