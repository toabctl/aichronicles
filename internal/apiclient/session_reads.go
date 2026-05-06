package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SessionEvents queries GET /v1/sessions/{id}/events. limit
// follows three-state semantics:
//   - 0 / unset: server-side default (DefaultEventsPerSessionLimit)
//   - >0:        explicit cap
//   - unbounded=true: every event in the session, no LIMIT clause
//     (used by the segmenter)
func (c *Client) SessionEvents(ctx context.Context, sessionID string, limit int, unbounded bool) (wire.SessionEventsResponse, error) {
	q := url.Values{}
	if unbounded {
		q.Set("unbounded", "true")
	} else if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/events"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.SessionEventsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.SessionEventsResponse{}, err
	}
	return out, nil
}

// SessionExtractions queries GET /v1/sessions/{id}/extractions?kind=.
// kind is required ("url", "file_path", "shell_command", ...).
func (c *Client) SessionExtractions(ctx context.Context, sessionID, kind string) (wire.SessionExtractionsResponse, error) {
	q := url.Values{}
	q.Set("kind", kind)
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/extractions?" + q.Encode()
	var out wire.SessionExtractionsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.SessionExtractionsResponse{}, err
	}
	return out, nil
}

// SessionCandidatePriors queries GET /v1/sessions/{id}/candidate-priors?limit=.
func (c *Client) SessionCandidatePriors(ctx context.Context, sessionID string, limit int) (wire.CandidateSessionListResponse, error) {
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/candidate-priors"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out wire.CandidateSessionListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
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
	q := url.Values{}
	if sinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(sinceMs, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/sessions/digests"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.SessionDigestsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.SessionDigestsResponse{}, err
	}
	return out, nil
}
