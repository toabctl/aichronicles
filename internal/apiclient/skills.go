package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SkillImpact queries GET /v1/skills/impact.
func (c *Client) SkillImpact(ctx context.Context, req wire.SkillImpactRequest) (wire.SkillImpactResponse, error) {
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
	var out wire.SkillImpactResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.SkillImpactResponse{}, err
	}
	return out, nil
}

// InstalledSkills queries GET /v1/skills/installed. The daemon walks
// ~/.claude/skills (global) and every project root derived from
// sessions newer than sinceMs (project-local) and returns the
// deduplicated, alphabetised slice.
func (c *Client) InstalledSkills(ctx context.Context, sinceMs int64) (wire.InstalledSkillsResponse, error) {
	q := url.Values{}
	if sinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(sinceMs, 10))
	}
	path := "/v1/skills/installed"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.InstalledSkillsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.InstalledSkillsResponse{}, err
	}
	return out, nil
}

// InvokedSkills queries GET /v1/skills/invoked. sinceMs of 0
// means "all time". Returns skills sorted by descending count.
func (c *Client) InvokedSkills(ctx context.Context, sinceMs int64) (wire.InvokedSkillsResponse, error) {
	q := url.Values{}
	if sinceMs > 0 {
		q.Set("since_ms", strconv.FormatInt(sinceMs, 10))
	}
	path := "/v1/skills/invoked"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out wire.InvokedSkillsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.InvokedSkillsResponse{}, err
	}
	return out, nil
}

// SkillStaleness queries GET /v1/skills/staleness.
func (c *Client) SkillStaleness(ctx context.Context, req wire.SkillStalenessRequest) (wire.SkillStalenessResponse, error) {
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
	var out wire.SkillStalenessResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return wire.SkillStalenessResponse{}, err
	}
	return out, nil
}
