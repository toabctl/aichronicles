package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Sessions queries GET /v1/sessions.
func (c *Client) Sessions(ctx context.Context, req api.SessionListRequest) (api.SessionListResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}

	path := "/v1/sessions"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out api.SessionListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SessionListResponse{}, err
	}
	return out, nil
}

// Session fetches a single session by id (GET /v1/sessions/{id}).
// Returns ErrNotFound when the session does not exist.
func (c *Client) Session(ctx context.Context, id string) (api.SessionDigest, error) {
	var out api.SessionDigest
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &out); err != nil {
		return api.SessionDigest{}, err
	}
	return out, nil
}

// RelatedSessions queries GET /v1/sessions/{id}/related.
func (c *Client) RelatedSessions(ctx context.Context, id string, limit int) (api.CandidateSessionListResponse, error) {
	path := "/v1/sessions/" + url.PathEscape(id) + "/related"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out api.CandidateSessionListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.CandidateSessionListResponse{}, err
	}
	return out, nil
}
