package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// FactSubjects queries GET /v1/facts/subjects?contains=&limit=.
func (c *Client) FactSubjects(ctx context.Context, contains string, limit int) (wire.FactSubjectsResponse, error) {
	q := url.Values{}
	if contains != "" {
		q.Set("contains", contains)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/facts/subjects"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.FactSubjectsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.FactSubjectsResponse{}, err
	}
	return out, nil
}

// Facts queries GET /v1/facts?subject=&limit=. Empty subject
// returns recent facts across all subjects.
func (c *Client) Facts(ctx context.Context, subject string, limit int) (wire.FactsResponse, error) {
	q := url.Values{}
	if subject != "" {
		q.Set("subject", subject)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/facts"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.FactsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.FactsResponse{}, err
	}
	return out, nil
}
