package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// SessionsMissingSummary queries GET /v1/sessions/missing-summary.
func (c *Client) SessionsMissingSummary(ctx context.Context, req api.SessionsMissingSummaryRequest) (api.SessionsMissingSummaryResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.Cwd != "" {
		q.Set("cwd", req.Cwd)
	}
	if req.Agent != "" {
		q.Set("agent", req.Agent)
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/sessions/missing-summary"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.SessionsMissingSummaryResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SessionsMissingSummaryResponse{}, err
	}
	return out, nil
}

// SessionsNeedingSegmentation queries
// GET /v1/sessions/needing-segmentation.
func (c *Client) SessionsNeedingSegmentation(ctx context.Context, req api.SessionsNeedingSegmentationRequest) (api.SessionsNeedingSegmentationResponse, error) {
	q := url.Values{}
	if req.IdleCutoffMs > 0 {
		q.Set("idle_cutoff_ms", strconv.FormatInt(req.IdleCutoffMs, 10))
	}
	if req.IdleMs > 0 {
		q.Set("idle_ms", strconv.FormatInt(req.IdleMs, 10))
	}
	if req.MinEvents > 0 {
		q.Set("min_events", strconv.Itoa(req.MinEvents))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/sessions/needing-segmentation"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.SessionsNeedingSegmentationResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SessionsNeedingSegmentationResponse{}, err
	}
	return out, nil
}

// SessionsForCompletion queries GET /v1/sessions/completions.
func (c *Client) SessionsForCompletion(ctx context.Context, prefix string, limit int) (api.SessionCompletionsResponse, error) {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/sessions/completions"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.SessionCompletionsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SessionCompletionsResponse{}, err
	}
	return out, nil
}

// SegmentSession posts to POST /v1/sessions/{id}/segment. Reads
// every event for the session, runs the segmenter server-side,
// writes the resulting episodes via SaveEpisodes, and returns the
// count produced.
func (c *Client) SegmentSession(ctx context.Context, sessionID string, req api.SegmentSessionRequest) (api.SegmentSessionResponse, error) {
	var out api.SegmentSessionResponse
	if err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/segment", req, &out); err != nil {
		return api.SegmentSessionResponse{}, err
	}
	return out, nil
}

// InductionCandidates queries GET /v1/induction/candidates.
func (c *Client) InductionCandidates(ctx context.Context, req api.InductionCandidatesRequest) (api.InductionCandidatesResponse, error) {
	q := url.Values{}
	if req.NowMs > 0 {
		q.Set("now_ms", strconv.FormatInt(req.NowMs, 10))
	}
	if req.IdleThresholdMs > 0 {
		q.Set("idle_threshold_ms", strconv.FormatInt(req.IdleThresholdMs, 10))
	}
	if req.MinEvents > 0 {
		q.Set("min_events", strconv.Itoa(req.MinEvents))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/induction/candidates"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.InductionCandidatesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.InductionCandidatesResponse{}, err
	}
	return out, nil
}

// FailureShapes queries GET /v1/proposals/failure-shapes.
func (c *Client) FailureShapes(ctx context.Context, sinceMs int64, limit int) (api.FailureShapesResponse, error) {
	q := url.Values{}
	if sinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(sinceMs, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/proposals/failure-shapes"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.FailureShapesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.FailureShapesResponse{}, err
	}
	return out, nil
}

// SkillFailures queries GET /v1/skills/failures.
func (c *Client) SkillFailures(ctx context.Context, req api.SkillFailuresRequest) (api.SkillFailuresResponse, error) {
	q := url.Values{}
	q.Set("skill", req.Skill)
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.WindowMs > 0 {
		q.Set("window_ms", strconv.FormatInt(req.WindowMs, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	var out api.SkillFailuresResponse
	if err := c.do(ctx, http.MethodGet, "/v1/skills/failures?"+q.Encode(), nil, &out); err != nil {
		return api.SkillFailuresResponse{}, err
	}
	return out, nil
}

// SkillCandidatesEffectiveness queries
// GET /v1/skill-candidates/effectiveness.
func (c *Client) SkillCandidatesEffectiveness(ctx context.Context, req api.SkillCandidateEffectivenessRequest) (api.SkillCandidateEffectivenessResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.WindowMs > 0 {
		q.Set("window_ms", strconv.FormatInt(req.WindowMs, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/skill-candidates/effectiveness"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.SkillCandidateEffectivenessResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SkillCandidateEffectivenessResponse{}, err
	}
	return out, nil
}

// SkillCandidatesPending queries GET /v1/skill-candidates/pending.
func (c *Client) SkillCandidatesPending(ctx context.Context, sinceMs int64, limit int) (api.PendingSkillCandidatesResponse, error) {
	q := url.Values{}
	if sinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(sinceMs, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/skill-candidates/pending"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.PendingSkillCandidatesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.PendingSkillCandidatesResponse{}, err
	}
	return out, nil
}

// AddedSkillCandidate queries GET /v1/skill-candidates/added?name=.
// ErrNotFound when no candidate row exists for the named skill
// (the skill is hand-authored).
func (c *Client) AddedSkillCandidate(ctx context.Context, name string) (api.SkillCandidate, error) {
	q := url.Values{}
	q.Set("name", name)
	var out api.AddedSkillCandidateResponse
	if err := c.do(ctx, http.MethodGet, "/v1/skill-candidates/added?"+q.Encode(), nil, &out); err != nil {
		return api.SkillCandidate{}, err
	}
	return out.Candidate, nil
}

// UpdateSkillCandidate posts to /v1/skill-candidates/{id}/update.
// Used by the merge path to converge the surviving candidate's
// stored hash + kind to the post-merge SKILL.md on disk.
func (c *Client) UpdateSkillCandidate(ctx context.Context, id int64, req api.UpdateSkillCandidateRequest) error {
	var out api.UpdateSkillCandidateResponse
	return c.do(ctx, http.MethodPost,
		"/v1/skill-candidates/"+strconv.FormatInt(id, 10)+"/update", req, &out)
}

// Vacuum posts to POST /v1/admin/vacuum. Synchronous: VACUUM
// holds an exclusive lock and the caller wants to know it
// finished before issuing follow-up work.
func (c *Client) Vacuum(ctx context.Context) error {
	var out api.VacuumResponse
	return c.do(ctx, http.MethodPost, "/v1/admin/vacuum", nil, &out)
}

// DBInfo queries GET /v1/admin/db-info — page_count + page_size +
// computed bytes for the live store.
func (c *Client) DBInfo(ctx context.Context) (api.DBPageInfoResponse, error) {
	var out api.DBPageInfoResponse
	if err := c.do(ctx, http.MethodGet, "/v1/admin/db-info", nil, &out); err != nil {
		return api.DBPageInfoResponse{}, err
	}
	return out, nil
}
