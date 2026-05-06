package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Events queries GET /v1/events with the supplied filters and
// returns the typed response. Any zero-valued field on the
// request is omitted from the query string so server-side
// defaults apply.
//
// Pagination: clients walk forward by passing the highest
// IngestSeq from the previous response as SinceSeq on the next
// call. The response carries LatestSeq so a client can detect
// "caught up" without a separate query.
func (c *Client) Events(ctx context.Context, req wire.EventListRequest) (wire.EventListResponse, error) {
	q := url.Values{}
	if req.SessionID != "" {
		q.Set("session_id", req.SessionID)
	}
	if req.SinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(req.SinceSeq, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}

	path := "/v1/events"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out wire.EventListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.EventListResponse{}, err
	}
	return out, nil
}
