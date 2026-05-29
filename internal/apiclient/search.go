package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Search queries GET /v1/search. Q is required; the server
// returns 400 when it is empty.
func (c *Client) Search(ctx context.Context, req wire.SearchRequest) (wire.SearchResponse, error) {
	var q qparams
	q.SetString("q", req.Q)
	q.SetString("kind", req.Kind)
	q.SetString("session_id", req.SessionID)
	q.SetString("subagent_id", req.SubagentID)
	q.SetString("source_agent", req.SourceAgent)
	q.SetString("tool_name", req.ToolName)
	q.SetString("skill_name", req.SkillName)
	q.SetString("file_path_substring", req.FilePathSubstring)
	q.SetBool("with_failures", req.WithFailures)
	q.SetBool("no_dedup", req.NoDedup)
	q.SetInt64("since_ms", req.SinceMs)
	q.SetInt("limit", req.Limit)
	q.SetString("cursor", string(req.Cursor))

	var out wire.SearchResponse
	if err := c.do(ctx, http.MethodGet, q.URL("/v1/search"), nil, &out); err != nil {
		return wire.SearchResponse{}, err
	}
	return out, nil
}
