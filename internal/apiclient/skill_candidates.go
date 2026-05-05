package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// RecordSkillCandidate posts to /v1/skill-candidates. Idempotent
// on (llm_output_id, skill_name) — re-posting with new metadata
// upserts metadata onto the existing row.
func (c *Client) RecordSkillCandidate(ctx context.Context, req api.RecordSkillCandidateRequest) (api.RecordSkillCandidateResponse, error) {
	var out api.RecordSkillCandidateResponse
	if err := c.do(ctx, http.MethodPost, "/v1/skill-candidates", req, &out); err != nil {
		return api.RecordSkillCandidateResponse{}, err
	}
	return out, nil
}

// SkillCandidateDecision posts to /v1/skill-candidates/decision.
// Decision is one of "add" | "merge" | "discard"; ErrNotFound
// surfaces when the (llm_output_id, skill_name) row was never
// recorded (caller forgot to RecordSkillCandidate first).
func (c *Client) SkillCandidateDecision(ctx context.Context, req api.SkillCandidateDecisionRequest) error {
	var out api.SkillCandidateDecisionResponse
	return c.do(ctx, http.MethodPost, "/v1/skill-candidates/decision", req, &out)
}

// SkillCandidatesByName queries GET /v1/skill-candidates?name=&limit=.
// Returns every candidate row matching the natural key, newest
// proposed first. limit <= 0 defaults to the server-side cap.
func (c *Client) SkillCandidatesByName(ctx context.Context, name string, limit int) (api.SkillCandidatesResponse, error) {
	q := url.Values{}
	q.Set("name", name)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out api.SkillCandidatesResponse
	if err := c.do(ctx, http.MethodGet, "/v1/skill-candidates?"+q.Encode(), nil, &out); err != nil {
		return api.SkillCandidatesResponse{}, err
	}
	return out, nil
}
