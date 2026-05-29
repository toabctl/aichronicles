package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SessionEvents queries GET /v1/sessions/{id}/events. limit
// follows three-state semantics:
//   - 0 / unset: server-side default (DefaultEventsPerSessionLimit)
//   - >0:        explicit cap
//   - unbounded=true: every event in the session, no LIMIT clause
//     (used by the segmenter)
func (c *Client) SessionEvents(ctx context.Context, sessionID string, limit int, unbounded bool) (wire.SessionEventsResponse, error) {
	var q qparams
	q.SetBool("unbounded", unbounded)
	if !unbounded {
		q.SetInt("limit", limit)
	}
	var out wire.SessionEventsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/"+url.PathEscape(sessionID)+"/events"), nil, &out); err != nil {
		return wire.SessionEventsResponse{}, err
	}
	return out, nil
}

// SessionExtractions queries GET /v1/sessions/{id}/extractions?kind=.
// kind is required ("url", "file_path", "shell_command", ...).
func (c *Client) SessionExtractions(ctx context.Context, sessionID, kind string) (wire.SessionExtractionsResponse, error) {
	var q qparams
	q.SetString("kind", kind)
	var out wire.SessionExtractionsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/"+url.PathEscape(sessionID)+"/extractions"), nil, &out); err != nil {
		return wire.SessionExtractionsResponse{}, err
	}
	return out, nil
}

// SessionCandidatePriors queries GET /v1/sessions/{id}/candidate-priors?limit=.
func (c *Client) SessionCandidatePriors(ctx context.Context, sessionID string, limit int) (wire.CandidateSessionListResponse, error) {
	var q qparams
	q.SetInt("limit", limit)
	var out wire.CandidateSessionListResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/"+url.PathEscape(sessionID)+"/candidate-priors"), nil, &out); err != nil {
		return wire.CandidateSessionListResponse{}, err
	}
	return out, nil
}

// SessionOutcome queries GET /v1/sessions/{id}/outcome. Read-or-
// backfill: the first call computes + persists; subsequent calls
// hit the cached row.
func (c *Client) SessionOutcome(ctx context.Context, sessionID string) (wire.SessionOutcome, error) {
	var out wire.SessionOutcome
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/outcome", nil, &out); err != nil {
		return wire.SessionOutcome{}, err
	}
	return out, nil
}

// SessionDigests queries GET /v1/sessions/digests?since_ms=&limit=.
// Returns the LoadRecentSessionDigests result — full SessionDigest
// rows for reflect/propose's window. Distinct from
// c.Sessions which serves the cwd-scoped MCP path.
func (c *Client) SessionDigests(ctx context.Context, sinceMs int64, limit int) (wire.SessionDigestsResponse, error) {
	var q qparams
	q.SetInt64("since_ms", sinceMs)
	q.SetInt("limit", limit)
	var out wire.SessionDigestsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/digests"), nil, &out); err != nil {
		return wire.SessionDigestsResponse{}, err
	}
	return out, nil
}

// SessionDigestsByIDs queries GET
// /v1/sessions/digests?session_ids=id1,id2,… and returns the
// digests keyed by session id. Sessions not found are simply absent
// from the map. An empty ids slice skips the call and returns an
// empty map — the caller has nothing to resolve.
func (c *Client) SessionDigestsByIDs(ctx context.Context, ids []string) (map[string]wire.SessionDigest, error) {
	if len(ids) == 0 {
		return map[string]wire.SessionDigest{}, nil
	}
	var q qparams
	q.SetString("session_ids", strings.Join(ids, ","))
	var out wire.SessionDigestsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/digests"), nil, &out); err != nil {
		return nil, err
	}
	byID := make(map[string]wire.SessionDigest, len(out.Digests))
	for _, d := range out.Digests {
		byID[d.ID] = d
	}
	return byID, nil
}
