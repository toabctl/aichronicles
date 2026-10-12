package apiclient

import (
	"errors"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

// TestClient_Audit_ForwardsCursor proves Cursor reaches the server: a
// malformed cursor is a 400 there, so a client that dropped the field
// would get a 200 first page instead.
func TestClient_Audit_ForwardsCursor(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	_, err := c.Audit(t.Context(), wire.AuditRequest{Cursor: "!!!"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("got %v, want a 400 HTTPError", err)
	}
}

func TestClient_Audit_EmptyStoreIsLastPage(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	resp, err := c.Audit(t.Context(), wire.AuditRequest{})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if resp.Scanned != 0 || resp.NextCursor != "" {
		t.Errorf("empty store: got scanned=%d next_cursor=%q, want 0 and empty", resp.Scanned, resp.NextCursor)
	}
}
