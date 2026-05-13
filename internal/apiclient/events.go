package apiclient

import (
	"context"
	"net/http"
	"strings"

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
	var q qparams
	q.SetString("session_id", req.SessionID)
	q.SetInt64("since_seq", req.SinceSeq)
	q.SetInt("limit", req.Limit)
	var out wire.EventListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/events"), nil, &out); err != nil {
		return wire.EventListResponse{}, err
	}
	return out, nil
}

// EventsLatestBatch fetches each session's most recent event in one
// round-trip. Keyed by session_id; sessions with zero events are
// absent. Empty ids returns an empty map (no HTTP call).
func (c *Client) EventsLatestBatch(ctx context.Context, ids []string) (map[string]wire.Event, error) {
	if len(ids) == 0 {
		return map[string]wire.Event{}, nil
	}
	var q qparams
	q.SetString("session_ids", strings.Join(ids, ","))
	var out wire.LatestEventsBatchResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/events/latest"), nil, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}
