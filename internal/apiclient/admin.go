package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Scrub re-runs the redaction scanner over every stored row.
// Idempotent. With DryRun=true (the default zero-value), the
// server reports what would change without mutating; with
// DryRun=false the rewrites commit. The scrub holds SQLite's
// write lock for the scan duration, so the api blocks other
// writers (the hook ingest path) until it completes — operators
// run during quiet windows.
func (c *Client) Scrub(ctx context.Context, req api.ScrubRequest) (api.ScrubResponse, error) {
	var out api.ScrubResponse
	if err := c.do(ctx, http.MethodPost, "/v1/scrub", req, &out); err != nil {
		return api.ScrubResponse{}, err
	}
	return out, nil
}

// Prune deletes sessions older than CutoffMs and everything they
// own. Active sessions (ended_at NULL) are protected. The api
// rejects CutoffMs<=0 with 400 to prevent "prune everything" on
// a typo.
func (c *Client) Prune(ctx context.Context, req api.PruneRequest) (api.PruneResponse, error) {
	var out api.PruneResponse
	if err := c.do(ctx, http.MethodPost, "/v1/prune", req, &out); err != nil {
		return api.PruneResponse{}, err
	}
	return out, nil
}
