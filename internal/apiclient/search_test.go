package apiclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/toabctl/aichronicles/internal/wire"
)

// TestSearch_SendsCursorAndDecodesNextCursor pins the cursor plumbing:
// the request carries ?cursor= verbatim and the response's
// next_cursor decodes back onto the typed field. A stub handler echoes
// the received cursor so we can assert it round-tripped.
func TestSearch_SendsCursorAndDecodesNextCursor(t *testing.T) {
	t.Parallel()
	var gotCursor string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCursor = r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[],"next_cursor":"NEXT123"}`))
	}))

	resp, err := c.Search(context.Background(), wire.SearchRequest{Q: "x", Cursor: "PREV456"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotCursor != "PREV456" {
		t.Errorf("outgoing cursor: got %q, want PREV456", gotCursor)
	}
	if resp.NextCursor != "NEXT123" {
		t.Errorf("decoded NextCursor: got %q, want NEXT123", resp.NextCursor)
	}
}

// TestSearch_OmitsEmptyCursor confirms a first-page request (empty
// Cursor) does not send the param at all, so existing callers are
// unaffected.
func TestSearch_OmitsEmptyCursor(t *testing.T) {
	t.Parallel()
	hasCursor := true
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasCursor = r.URL.Query()["cursor"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[]}`))
	}))

	if _, err := c.Search(context.Background(), wire.SearchRequest{Q: "x"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if hasCursor {
		t.Error("empty cursor must not be sent as a query param")
	}
}
