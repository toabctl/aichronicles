package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// FactSubjects queries GET /v1/facts/subjects?contains=&limit=.
func (c *Client) FactSubjects(ctx context.Context, contains string, limit int) (wire.FactSubjectsResponse, error) {
	var q qparams
	q.SetString("contains", contains)
	q.SetInt("limit", limit)
	var out wire.FactSubjectsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/facts/subjects"), nil, &out); err != nil {
		return wire.FactSubjectsResponse{}, err
	}
	return out, nil
}

// Facts queries GET /v1/facts?subject=&limit=. Empty subject
// returns recent facts across all subjects.
func (c *Client) Facts(ctx context.Context, subject string, limit int) (wire.FactsResponse, error) {
	var q qparams
	q.SetString("subject", subject)
	q.SetInt("limit", limit)
	var out wire.FactsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/facts"), nil, &out); err != nil {
		return wire.FactsResponse{}, err
	}
	return out, nil
}
