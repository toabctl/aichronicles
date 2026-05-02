package apiclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"

	"github.com/toabctl/aichronicles/pkg/api"
)

// Sentinel errors. Callers prefer errors.Is(err, ErrNotFound)
// over string matching; structural errors (BadRequest with
// detail) are matched via errors.As.
var (
	// ErrSocketUnavailable wraps a connection-refused / no-such-
	// file failure when reaching the api UDS. Distinguishable so
	// CLIs can suggest "is aichronicles-api running?" instead of
	// printing a generic transport error.
	ErrSocketUnavailable = errors.New("apiclient: api socket unavailable")

	// ErrNotFound is the typed mapping of a 404 response.
	ErrNotFound = errors.New("apiclient: not found")

	// ErrTooLarge is the typed mapping of a 413 response.
	ErrTooLarge = errors.New("apiclient: request too large")

	// ErrServer is the typed mapping of a 5xx response.
	ErrServer = errors.New("apiclient: server error")

	// ErrConflict is the typed mapping of a 409 response — used
	// by write endpoints (e.g., concurrent admin operation,
	// unique-key collision).
	ErrConflict = errors.New("apiclient: conflict")
)

// HTTPError carries the structured detail of a non-2xx response so
// callers can choose between sentinel matching (errors.Is) and
// detailed inspection (errors.As). Status mirrors the HTTP code;
// Problem is the decoded RFC 7807 body when one was provided.
type HTTPError struct {
	Status  int
	Problem api.Problem
	// sentinel is the matchable error returned by Unwrap so
	// errors.Is(err, ErrNotFound) works against an *HTTPError.
	sentinel error
}

func (e *HTTPError) Error() string {
	if e.Problem.Title != "" && e.Problem.Detail != "" {
		return fmt.Sprintf("apiclient: %d %s: %s", e.Status, e.Problem.Title, e.Problem.Detail)
	}
	if e.Problem.Title != "" {
		return fmt.Sprintf("apiclient: %d %s", e.Status, e.Problem.Title)
	}
	return fmt.Sprintf("apiclient: HTTP %d", e.Status)
}

// Unwrap returns a sentinel error so errors.Is matches the
// canonical class (ErrNotFound, ErrTooLarge, ...).
func (e *HTTPError) Unwrap() error { return e.sentinel }

// decodeProblem reads a non-2xx response and returns the typed
// error. A missing or malformed problem+json body still produces
// a useful HTTPError carrying the status code.
func decodeProblem(resp *http.Response) error {
	herr := &HTTPError{
		Status:   resp.StatusCode,
		sentinel: sentinelForStatus(resp.StatusCode),
	}
	body, err := io.ReadAll(resp.Body)
	if err == nil && len(body) > 0 {
		// Tolerate malformed problem bodies: we still surface the
		// status, just without the title/detail.
		_ = json.Unmarshal(body, &herr.Problem)
	}
	return herr
}

// sentinelForStatus maps an HTTP status to a canonical sentinel
// for errors.Is matching. Unknown 4xx falls through to a generic
// client-side error; unknown 5xx maps to ErrServer.
func sentinelForStatus(status int) error {
	switch status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusConflict:
		return ErrConflict
	}
	if status >= 500 {
		return ErrServer
	}
	// 4xx other than the ones above → no sentinel match; callers
	// can still inspect the HTTPError.
	return nil
}

// wrapTransportError translates http.Client.Do errors into
// actionable apiclient errors. The interesting case is "the UDS
// file doesn't exist or refuses connections," which we surface
// as ErrSocketUnavailable so CLIs can prompt the operator.
func wrapTransportError(err error, baseURL, path string) error {
	if err == nil {
		return nil
	}
	if isSocketUnavailable(err) {
		return fmt.Errorf("apiclient: %w (target=%s%s): %v", ErrSocketUnavailable, baseURL, path, err)
	}
	return fmt.Errorf("apiclient: transport (target=%s%s): %w", baseURL, path, err)
}

// isSocketUnavailable detects the common "daemon not running"
// failure modes for a UDS dial: ENOENT (no such file), ECONNREFUSED
// (no listener), and the wrapped variants the net package
// produces.
func isSocketUnavailable(err error) bool {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// net errors sometimes carry the syscall error in a string
	// (older Go versions or wrapping mismatches). Treat the
	// canonical messages as a fallback signal.
	msg := err.Error()
	return strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "connection refused")
}
