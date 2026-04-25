package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// Client speaks to the aichroniclesd HTTP API over a Unix domain socket.
type Client struct {
	SocketPath string
	HTTP       *http.Client
}

// NewClient returns a Client wired to dial the given UDS path.
// The caller is responsible for setting an appropriate overall deadline
// via the context passed to Post.
func NewClient(sockPath string) *Client {
	return &Client{
		SocketPath: sockPath,
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	}
}

// Post sends env to /v1/ingest. On success returns the Ack; on a non-2xx
// response returns an error carrying the body (which is expected to be
// an RFC 7807 Problem document).
func (c *Client) Post(ctx context.Context, env ingest.Envelope) (ingest.Ack, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return ingest.Ack{}, fmt.Errorf("marshal envelope: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/ingest", bytes.NewReader(body))
	if err != nil {
		return ingest.Ack{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ingest.Ack{}, fmt.Errorf("post to daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ingest.Ack{}, fmt.Errorf("daemon rejected envelope (%d): %s", resp.StatusCode, bytes.TrimSpace(buf))
	}

	var ack ingest.Ack
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return ingest.Ack{}, fmt.Errorf("decode ack: %w", err)
	}
	return ack, nil
}
