package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Scrub re-runs the redaction scanner over every stored row.
// Idempotent. DryRun is required (nil is a 400): true reports what
// would change without mutating; false commits the rewrites. The scrub holds SQLite's
// write lock for the scan duration, so the api blocks other
// writers (the hook ingest path) until it completes — operators
// run during quiet windows.
func (c *Client) Scrub(ctx context.Context, req wire.ScrubRequest) (wire.ScrubResponse, error) {
	var out wire.ScrubResponse
	if err := c.do(ctx, http.MethodPost, "/v1/scrub", req, &out); err != nil {
		return wire.ScrubResponse{}, err
	}
	return out, nil
}

// Prune deletes sessions older than CutoffMs and everything they
// own. Active sessions (ended_at NULL) are protected. DryRun is
// required (nil is a 400), and the api rejects a CutoffMs that is
// <= 0 or in the future with 400 — either would prune every ended
// session.
func (c *Client) Prune(ctx context.Context, req wire.PruneRequest) (wire.PruneResponse, error) {
	var out wire.PruneResponse
	if err := c.do(ctx, http.MethodPost, "/v1/prune", req, &out); err != nil {
		return wire.PruneResponse{}, err
	}
	return out, nil
}
