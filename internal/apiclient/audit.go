package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Audit queries one page of GET /v1/audit. The server runs the
// canonical redact pattern set against events with non-null
// content_text, most-recently-ingested first, and returns one finding
// per matched event plus the page's aggregate counters. A non-empty
// NextCursor means rows remain: pass it back as req.Cursor (with the
// same SinceMs) to continue. Snippet bytes never carry the raw
// secret — every matched span is rendered as <pattern> on the wire.
func (c *Client) Audit(ctx context.Context, req wire.AuditRequest) (wire.AuditResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt("limit", req.Limit)
	q.SetString("cursor", string(req.Cursor))
	var out wire.AuditResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/audit"), nil, &out); err != nil {
		return wire.AuditResponse{}, err
	}
	return out, nil
}
