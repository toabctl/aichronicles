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
	if req.SourceAgent != "" {
		q.Set("source_agent", req.SourceAgent)
	}
	if req.Project != "" {
		q.Set("project", req.Project)
	}
	if req.ToolName != "" {
		q.Set("tool_name", req.ToolName)
	}
	if req.SkillName != "" {
		q.Set("skill_name", req.SkillName)
	}
	if req.FilePathSubstring != "" {
		q.Set("file_path_substring", req.FilePathSubstring)
	}
	if req.WithFailures {
		q.Set("with_failures", "true")
	}
	if req.WithoutSummary {
		q.Set("without_summary", "true")
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

// SourceAgents queries GET /v1/sessions/source-agents. Distinct
// sessions.source_agent values, alphabetised — used by the web's
// faceted session-list picker.
func (c *Client) SourceAgents(ctx context.Context) ([]string, error) {
	var out wire.SourceAgentsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/source-agents", nil, &out); err != nil {
		return nil, err
	}
	return out.SourceAgents, nil
}

// SessionStartCwd queries GET /v1/sessions/{id}/start-cwd. Returns
// (nil, nil) when the session has no recorded start cwd; the consumer
// (typically a "resume" affordance) decides whether to fall back to
// sessions.cwd or hide the button. Returns ErrNotFound only when the
// session id itself doesn't exist.
func (c *Client) SessionStartCwd(ctx context.Context, id string) (*string, error) {
	var out wire.SessionStartCwdResponse
	if err := c.do(ctx, http.MethodGet,
		"/v1/sessions/"+url.PathEscape(id)+"/start-cwd", nil, &out); err != nil {
		return nil, err
	}
	return out.Cwd, nil
}

// SessionLinksFrom queries GET /v1/session-links?from=X — outgoing
// links from a session. Returns an empty slice (not nil) when none
// exist so renderers can iterate without nil checks.
func (c *Client) SessionLinksFrom(ctx context.Context, id string) ([]wire.SessionLinkRow, error) {
	q := url.Values{}
	q.Set("from", id)
	var out wire.SessionLinksResponse
	if err := c.do(ctx, http.MethodGet, "/v1/session-links?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Links, nil
}

// SessionLinksTo queries GET /v1/session-links?to=X — incoming
// reverse-index of links pointing AT a session.
func (c *Client) SessionLinksTo(ctx context.Context, id string) ([]wire.SessionLinkRow, error) {
	q := url.Values{}
	q.Set("to", id)
	var out wire.SessionLinksResponse
	if err := c.do(ctx, http.MethodGet, "/v1/session-links?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Links, nil
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
