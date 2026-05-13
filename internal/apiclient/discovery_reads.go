package apiclient

import (
	"context"
	"net/http"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SessionsMissingSummary queries GET /v1/sessions/missing-summary.
func (c *Client) SessionsMissingSummary(ctx context.Context, req wire.SessionsMissingSummaryRequest) (wire.SessionsMissingSummaryResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetString("cwd", req.Cwd)
	q.SetString("agent", req.Agent)
	q.SetInt("limit", req.Limit)
	var out wire.SessionsMissingSummaryResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/missing-summary"), nil, &out); err != nil {
		return wire.SessionsMissingSummaryResponse{}, err
	}
	return out, nil
}

// SessionsNeedingSegmentation queries
// GET /v1/sessions/needing-segmentation.
func (c *Client) SessionsNeedingSegmentation(ctx context.Context, req wire.SessionsNeedingSegmentationRequest) (wire.SessionsNeedingSegmentationResponse, error) {
	var q qparams
	q.SetInt64("idle_cutoff_ms", req.IdleCutoffMs)
	q.SetInt64("idle_ms", req.IdleMs)
	q.SetInt("min_events", req.MinEvents)
	q.SetInt("limit", req.Limit)
	var out wire.SessionsNeedingSegmentationResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/needing-segmentation"), nil, &out); err != nil {
		return wire.SessionsNeedingSegmentationResponse{}, err
	}
	return out, nil
}

// SessionsForCompletion queries GET /v1/sessions/completions.
func (c *Client) SessionsForCompletion(ctx context.Context, prefix string, limit int) (wire.SessionCompletionsResponse, error) {
	var q qparams
	q.SetString("prefix", prefix)
	q.SetInt("limit", limit)
	var out wire.SessionCompletionsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/sessions/completions"), nil, &out); err != nil {
		return wire.SessionCompletionsResponse{}, err
	}
	return out, nil
}

// SegmentSession posts to POST /v1/sessions/{id}/segment. Reads
// every event for the session, runs the segmenter server-side,
// writes the resulting episodes via SaveEpisodes, and returns the
// count produced.
func (c *Client) SegmentSession(ctx context.Context, sessionID string, req wire.SegmentSessionRequest) (wire.SegmentSessionResponse, error) {
	var out wire.SegmentSessionResponse
	if err := c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/segment", req, &out); err != nil {
		return wire.SegmentSessionResponse{}, err
	}
	return out, nil
}

// InductionCandidates queries GET /v1/induction/candidates.
func (c *Client) InductionCandidates(ctx context.Context, req wire.InductionCandidatesRequest) (wire.InductionCandidatesResponse, error) {
	var q qparams
	q.SetInt64("now_ms", req.NowMs)
	q.SetInt64("idle_threshold_ms", req.IdleThresholdMs)
	q.SetInt("min_events", req.MinEvents)
	q.SetInt("limit", req.Limit)
	var out wire.InductionCandidatesResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/induction/candidates"), nil, &out); err != nil {
		return wire.InductionCandidatesResponse{}, err
	}
	return out, nil
}

// FailureShapes queries GET /v1/proposals/failure-shapes.
func (c *Client) FailureShapes(ctx context.Context, sinceMs int64, limit int) (wire.FailureShapesResponse, error) {
	var q qparams
	q.SetInt64("since_ms", sinceMs)
	q.SetInt("limit", limit)
	var out wire.FailureShapesResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/proposals/failure-shapes"), nil, &out); err != nil {
		return wire.FailureShapesResponse{}, err
	}
	return out, nil
}

// SkillFailures queries GET /v1/skills/failures.
func (c *Client) SkillFailures(ctx context.Context, req wire.SkillFailuresRequest) (wire.SkillFailuresResponse, error) {
	var q qparams
	q.SetString("skill", req.Skill)
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt64("window_ms", req.WindowMs)
	q.SetInt("limit", req.Limit)
	var out wire.SkillFailuresResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skills/failures"), nil, &out); err != nil {
		return wire.SkillFailuresResponse{}, err
	}
	return out, nil
}

// SkillCandidatesEffectiveness queries
// GET /v1/skill-candidates/effectiveness.
func (c *Client) SkillCandidatesEffectiveness(ctx context.Context, req wire.SkillCandidateEffectivenessRequest) (wire.SkillCandidateEffectivenessResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt64("window_ms", req.WindowMs)
	q.SetInt("limit", req.Limit)
	var out wire.SkillCandidateEffectivenessResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skill-candidates/effectiveness"), nil, &out); err != nil {
		return wire.SkillCandidateEffectivenessResponse{}, err
	}
	return out, nil
}

// SkillCandidatesPending queries GET /v1/skill-candidates/pending.
func (c *Client) SkillCandidatesPending(ctx context.Context, sinceMs int64, limit int) (wire.PendingSkillCandidatesResponse, error) {
	var q qparams
	q.SetInt64("since_ms", sinceMs)
	q.SetInt("limit", limit)
	var out wire.PendingSkillCandidatesResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skill-candidates/pending"), nil, &out); err != nil {
		return wire.PendingSkillCandidatesResponse{}, err
	}
	return out, nil
}

// AddedSkillCandidate queries GET /v1/skill-candidates/added?name=.
// ErrNotFound when no candidate row exists for the named skill
// (the skill is hand-authored).
func (c *Client) AddedSkillCandidate(ctx context.Context, name string) (wire.SkillCandidate, error) {
	var q qparams
	q.SetString("name", name)
	var out wire.AddedSkillCandidateResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skill-candidates/added"), nil, &out); err != nil {
		return wire.SkillCandidate{}, err
	}
	return out.Candidate, nil
}

// UpdateSkillCandidate posts to /v1/skill-candidates/{id}/update.
// Used by the merge path to converge the surviving candidate's
// stored hash + kind to the post-merge SKILL.md on disk.
func (c *Client) UpdateSkillCandidate(ctx context.Context, id int64, req wire.UpdateSkillCandidateRequest) error {
	var out wire.UpdateSkillCandidateResponse
	return c.do(ctx, http.MethodPost,
		"/v1/skill-candidates/"+strconv.FormatInt(id, 10)+"/update", req, &out)
}

// Vacuum posts to POST /v1/admin/vacuum. Synchronous: VACUUM
// holds an exclusive lock and the caller wants to know it
// finished before issuing follow-up work.
func (c *Client) Vacuum(ctx context.Context) error {
	var out wire.VacuumResponse
	return c.do(ctx, http.MethodPost, "/v1/admin/vacuum", nil, &out)
}

// DBInfo queries GET /v1/admin/db-info — page_count + page_size +
// computed bytes for the live store.
func (c *Client) DBInfo(ctx context.Context) (wire.DBPageInfoResponse, error) {
	var out wire.DBPageInfoResponse
	if err := c.do(ctx, http.MethodGet, "/v1/admin/db-info", nil, &out); err != nil {
		return wire.DBPageInfoResponse{}, err
	}
	return out, nil
}
