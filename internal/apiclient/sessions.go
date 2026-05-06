package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Sessions queries GET /v1/sessions.
func (c *Client) Sessions(ctx context.Context, req wire.SessionListRequest) (wire.SessionListResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.Cwd != "" {
		q.Set("cwd", req.Cwd)
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}

	path := "/v1/sessions"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out wire.SessionListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.SessionListResponse{}, err
	}
	return out, nil
}

// Session fetches a single session by id (GET /v1/sessions/{id}).
// Returns ErrNotFound when the session does not exist.
func (c *Client) Session(ctx context.Context, id string) (wire.SessionDigest, error) {
	var out wire.SessionDigest
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &out); err != nil {
		return wire.SessionDigest{}, err
	}
	return out, nil
}

// ResolveSession queries GET /v1/sessions/resolve?prefix=. Maps
// an 8-or-more-character hex prefix to a canonical session_id;
// ErrNotFound when no session matches and ErrConflict when the
// prefix is ambiguous (multiple matches). Used by CLIs and the
// MCP server to accept short prefixes from humans.
func (c *Client) ResolveSession(ctx context.Context, prefix string) (string, error) {
	q := url.Values{}
	q.Set("prefix", prefix)
	var out wire.ResolveSessionResponse
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/resolve?"+q.Encode(), nil, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// RelatedSessions queries GET /v1/sessions/{id}/related.
func (c *Client) RelatedSessions(ctx context.Context, id string, limit int) (wire.CandidateSessionListResponse, error) {
	path := "/v1/sessions/" + url.PathEscape(id) + "/related"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out wire.CandidateSessionListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.CandidateSessionListResponse{}, err
	}
	return out, nil
}
