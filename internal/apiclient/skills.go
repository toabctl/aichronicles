package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SkillImpact queries GET /v1/skills/impact.
func (c *Client) SkillImpact(ctx context.Context, req wire.SkillImpactRequest) (wire.SkillImpactResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt64("window_ms", req.WindowMs)
	q.SetInt("max_skills", req.MaxSkills)
	var out wire.SkillImpactResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skills/impact"), nil, &out); err != nil {
		return wire.SkillImpactResponse{}, err
	}
	return out, nil
}

// InstalledSkills queries GET /v1/skills/installed. The daemon walks
// ~/.claude/skills (global) and every project root derived from
// sessions newer than sinceMs (project-local) and returns the
// deduplicated, alphabetised slice.
func (c *Client) InstalledSkills(ctx context.Context, sinceMs int64) (wire.InstalledSkillsResponse, error) {
	var q qparams
	q.SetInt64("since_ms", sinceMs)
	var out wire.InstalledSkillsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skills/installed"), nil, &out); err != nil {
		return wire.InstalledSkillsResponse{}, err
	}
	return out, nil
}

// InvokedSkills queries GET /v1/skills/invoked. sinceMs of 0
// means "all time". Returns skills sorted by descending count.
func (c *Client) InvokedSkills(ctx context.Context, sinceMs int64) (wire.InvokedSkillsResponse, error) {
	var q qparams
	q.SetInt64("since_ms", sinceMs)
	var out wire.InvokedSkillsResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skills/invoked"), nil, &out); err != nil {
		return wire.InvokedSkillsResponse{}, err
	}
	return out, nil
}

// SkillStaleness queries GET /v1/skills/staleness.
func (c *Client) SkillStaleness(ctx context.Context, req wire.SkillStalenessRequest) (wire.SkillStalenessResponse, error) {
	var q qparams
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt64("window_ms", req.WindowMs)
	q.SetInt("max_skills", req.MaxSkills)
	q.SetInt("max_examples", req.MaxExamples)
	var out wire.SkillStalenessResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/skills/staleness"), nil, &out); err != nil {
		return wire.SkillStalenessResponse{}, err
	}
	return out, nil
}
