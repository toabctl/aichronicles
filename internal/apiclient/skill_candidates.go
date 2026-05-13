package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// RecordSkillCandidate posts to /v1/skill-candidates. Idempotent
// on (llm_output_id, skill_name) — re-posting with new metadata
// upserts metadata onto the existing row.
func (c *Client) RecordSkillCandidate(ctx context.Context, req wire.RecordSkillCandidateRequest) (wire.RecordSkillCandidateResponse, error) {
	var out wire.RecordSkillCandidateResponse
	if err := c.do(ctx, http.MethodPost, "/v1/skill-candidates", req, &out); err != nil {
		return wire.RecordSkillCandidateResponse{}, err
	}
	return out, nil
}

// SkillCandidateDecision posts to /v1/skill-candidates/decision.
// Decision is one of wire.DecisionAdd | DecisionMerge | DecisionDiscard;
// ErrNotFound surfaces when the (llm_output_id, skill_name) row was
// never recorded (caller forgot to RecordSkillCandidate first).
func (c *Client) SkillCandidateDecision(ctx context.Context, req wire.SkillCandidateDecisionRequest) error {
	var out wire.SkillCandidateDecisionResponse
	return c.do(ctx, http.MethodPost, "/v1/skill-candidates/decision", req, &out)
}

// SkillCandidatesByName queries GET /v1/skill-candidates?name=&limit=.
// Returns every candidate row matching the natural key, newest
// proposed first. limit <= 0 defaults to the server-side cap.
func (c *Client) SkillCandidatesByName(ctx context.Context, name string, limit int) (wire.SkillCandidatesResponse, error) {
	var q qparams
	q.SetString("name", name)
	q.SetInt("limit", limit)
	var out wire.SkillCandidatesResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skill-candidates"), nil, &out); err != nil {
		return wire.SkillCandidatesResponse{}, err
	}
	return out, nil
}
