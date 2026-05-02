// Package apiclient is the typed Go client for aichronicles-api.
//
// One Client per process; safe for concurrent use across many
// goroutines (the underlying *http.Client handles its own
// connection pooling). Client construction is cheap; reuse is
// preferred so the UDS connection pool warms up.
//
// Production wiring: NewClient resolves the socket path to the
// api daemon's UDS and configures an http.Client with a Unix
// dialer plus reasonable timeouts. Tests use newClientForTests
// to point at an httptest.Server (TCP) — the wire shape is the
// same, so test coverage is meaningful.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// defaultRequestTimeout caps every request's total wall time when
// the caller hasn't supplied a deadline via context. Long enough
// for a slow scrub or large search, short enough that a hung
// daemon can't wedge the CLI indefinitely.
const defaultRequestTimeout = 30 * time.Second

// Client is the typed entry point. Methods are added per feature
// in sibling files (events.go, sessions.go, …). do() is the
// generic transport that handles request body marshaling, problem+
// json error mapping, and response decoding.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient returns a Client configured to dial the api daemon's
// Unix-domain socket at sockPath. The underlying http.Client uses
// a custom DialContext so http://unix/<path> requests resolve to
// the UDS. The server-side base URL host is "unix" — opaque,
// chosen so error messages identify the transport.
func NewClient(sockPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   defaultRequestTimeout,
		},
		baseURL: "http://unix",
	}
}

// NewClientForTesting builds a Client that talks to baseURL via the
// supplied *http.Client. Exported for cross-package test wiring (cli
// subcommand tests stand up an httptest.Server holding the real
// internal/api handlers and need to point an apiclient.Client at
// that TCP URL). Production code uses NewClient.
func NewClientForTesting(httpClient *http.Client, baseURL string) *Client {
	return &Client{httpClient: httpClient, baseURL: baseURL}
}

// newClientForTests is the unexported package-internal alias kept
// so existing same-package tests don't churn on the rename.
func newClientForTests(httpClient *http.Client, baseURL string) *Client {
	return NewClientForTesting(httpClient, baseURL)
}

// do is the single transport entry point. Marshals body (when
// non-nil) as JSON, executes the request, maps non-2xx responses
// to typed errors via problem+json decoding, and decodes a 2xx
// response body into into when non-nil. A 204 No Content with
// into != nil is not an error — into is left untouched.
//
// The caller's context bounds the request; the http.Client.Timeout
// is the upper bound only.
func (c *Client) do(ctx context.Context, method, path string, body, into any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("apiclient: marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("apiclient: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if into != nil {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return wrapTransportError(err, c.baseURL, path)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return decodeProblem(resp)
	}
	if into == nil || resp.StatusCode == http.StatusNoContent {
		// Drain so the underlying connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
		return fmt.Errorf("apiclient: unexpected content-type %q (want application/json)", ct)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("apiclient: decode response: %w", err)
	}
	return nil
}

// Healthz probes GET /v1/healthz. Returns nil when the daemon
// answers 2xx; any other outcome (transport error, non-2xx) is
// returned as an error suitable for surfacing to the user.
// The body shape (currently {"status":"ok"}) is not inspected —
// 200 is the contract.
func (c *Client) Healthz(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/v1/healthz", nil, nil)
}

// isJSON reports whether the Content-Type's media type is JSON.
// Handles "application/json", "application/json; charset=utf-8",
// and the rare "text/json" variant.
func isJSON(ct string) bool {
	for _, candidate := range []string{"application/json", "text/json"} {
		if len(ct) >= len(candidate) && ct[:len(candidate)] == candidate {
			return true
		}
	}
	return false
}
