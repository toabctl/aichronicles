package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/toabctl/aichronicles/pkg/api"
)

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
