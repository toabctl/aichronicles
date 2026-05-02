package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// FactSubjects queries GET /v1/facts/subjects?contains=&limit=.
func (c *Client) FactSubjects(ctx context.Context, contains string, limit int) (api.FactSubjectsResponse, error) {
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
	var out api.FactSubjectsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.FactSubjectsResponse{}, err
	}
	return out, nil
}

// Facts queries GET /v1/facts?subject=&limit=. Empty subject
// returns recent facts across all subjects.
func (c *Client) Facts(ctx context.Context, subject string, limit int) (api.FactsResponse, error) {
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
	var out api.FactsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.FactsResponse{}, err
	}
	return out, nil
}
