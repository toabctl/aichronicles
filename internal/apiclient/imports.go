package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/toabctl/aichronicles/internal/wire"
)

// Import streams NDJSON envelopes to POST /v1/import. The reader
// is consumed line-by-line server-side; large transcripts (a
// gigabyte+) work without buffering the whole body in memory.
//
// On success returns the typed stats. On non-2xx the response
// includes partial stats in the Problem.Detail (see
// internal/api/handler_import.go::partialStatsDetail) — callers
// that care about exact mid-stream progress decode that detail
// to recover counts.
func (c *Client) Import(ctx context.Context, body io.Reader) (wire.ImportStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/import", body)
	if err != nil {
		return wire.ImportStats{}, fmt.Errorf("apiclient: build import request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return wire.ImportStats{}, wrapTransportError(err, c.baseURL, "/v1/import")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return wire.ImportStats{}, decodeProblem(resp)
	}
	var out wire.ImportStats
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return wire.ImportStats{}, fmt.Errorf("apiclient: decode import response: %w", err)
	}
	return out, nil
}
