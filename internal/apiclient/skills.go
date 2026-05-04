package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

// SkillImpact queries GET /v1/skills/impact.
func (c *Client) SkillImpact(ctx context.Context, req api.SkillImpactRequest) (api.SkillImpactResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.WindowMs > 0 {
		q.Set("window_ms", strconv.FormatInt(req.WindowMs, 10))
	}
	if req.MaxSkills > 0 {
		q.Set("max_skills", strconv.Itoa(req.MaxSkills))
	}
	path := "/v1/skills/impact"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.SkillImpactResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SkillImpactResponse{}, err
	}
	return out, nil
}

// InvokedSkills queries GET /v1/skills/invoked. sinceMs of 0
// means "all time". Returns skills sorted by descending count.
func (c *Client) InvokedSkills(ctx context.Context, sinceMs int64) (api.InvokedSkillsResponse, error) {
	q := url.Values{}
	if sinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(sinceMs, 10))
	}
	path := "/v1/skills/invoked"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.InvokedSkillsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.InvokedSkillsResponse{}, err
	}
	return out, nil
}

// SkillStaleness queries GET /v1/skills/staleness.
func (c *Client) SkillStaleness(ctx context.Context, req api.SkillStalenessRequest) (api.SkillStalenessResponse, error) {
	q := url.Values{}
	if req.SinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(req.SinceMs, 10))
	}
	if req.WindowMs > 0 {
		q.Set("window_ms", strconv.FormatInt(req.WindowMs, 10))
	}
	if req.MaxSkills > 0 {
		q.Set("max_skills", strconv.Itoa(req.MaxSkills))
	}
	if req.MaxExamples > 0 {
		q.Set("max_examples", strconv.Itoa(req.MaxExamples))
	}
	path := "/v1/skills/staleness"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.SkillStalenessResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return api.SkillStalenessResponse{}, err
	}
	return out, nil
}
