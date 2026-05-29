package apiclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Sessions queries GET /v1/sessions.
func (c *Client) Sessions(ctx context.Context, req wire.SessionListRequest) (wire.SessionListResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetString("cwd", req.Cwd)
	q.SetInt("limit", req.Limit)
	q.SetString("source_agent", req.SourceAgent)
	q.SetString("project", req.Project)
	q.SetString("tool_name", req.ToolName)
	q.SetString("skill_name", req.SkillName)
	q.SetString("file_path_substring", req.FilePathSubstring)
	q.SetBool("with_failures", req.WithFailures)
	q.SetBool("without_summary", req.WithoutSummary)
	q.SetString("cursor", string(req.Cursor))

	var out wire.SessionListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions"), nil, &out); err != nil {
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
	var q qparams
	q.SetString("prefix", prefix)
	var out wire.ResolveSessionResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/resolve"), nil, &out); err != nil {
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
	var q qparams
	q.SetString("from", id)
	var out wire.SessionLinksResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/session-links"), nil, &out); err != nil {
		return nil, err
	}
	return out.Links, nil
}

// SessionLinksTo queries GET /v1/session-links?to=X — incoming
// reverse-index of links pointing AT a session.
func (c *Client) SessionLinksTo(ctx context.Context, id string) ([]wire.SessionLinkRow, error) {
	var q qparams
	q.SetString("to", id)
	var out wire.SessionLinksResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/session-links"), nil, &out); err != nil {
		return nil, err
	}
	return out.Links, nil
}

// RelatedSessions queries GET /v1/sessions/{id}/related.
func (c *Client) RelatedSessions(ctx context.Context, id string, limit int) (wire.CandidateSessionListResponse, error) {
	var q qparams
	q.SetInt("limit", limit)
	var out wire.CandidateSessionListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/"+url.PathEscape(id)+"/related"), nil, &out); err != nil {
		return wire.CandidateSessionListResponse{}, err
	}
	return out, nil
}
