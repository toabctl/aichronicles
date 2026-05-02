package apiclient

import (
	"context"
	"net/http"

	"github.com/toabctl/aichronicles/pkg/events"
)

// Ingest POSTs an envelope to /v1/ingest. Returns the server's
// Ack on success, or a typed error on failure (ErrTooLarge for
// 413, ErrSocketUnavailable when the daemon is not running, an
// HTTPError for problem+json responses).
//
// The Pipeline on the server applies redaction, so callers do
// not pre-redact. Validate is also performed server-side. The
// body posted here is exactly what the daemon stores in
// raw_envelopes (after server-side redaction).
func (c *Client) Ingest(ctx context.Context, env events.Envelope) (events.Ack, error) {
	var ack events.Ack
	if err := c.do(ctx, http.MethodPost, "/v1/ingest", env, &ack); err != nil {
		return events.Ack{}, err
	}
	return ack, nil
}
