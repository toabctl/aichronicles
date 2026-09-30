package apiclient

import (
	"context"
	"fmt"

	"github.com/toabctl/aichronicles/internal/wire"
)

// collectPages walks a paginated list endpoint for a caller that
// wants "all of it" rather than one page. fetch returns one page of
// at most limit items and its next cursor; collectPages follows
// NextCursor — the server's only end-of-list signal — until it comes
// back empty or max items are collected (max <= 0 means no cap).
//
// truncated reports, exactly, that the list holds more than max items:
// collectPages asks for one item past the cap rather than guessing
// from a non-empty cursor, which a server also returns when the next
// page happens to be empty. Consumers must surface truncated; the bug
// class this exists for is a tool presenting a silently cut first
// page as the complete answer.
//
// A cursor that fails to advance is an error rather than a loop.
func collectPages[T any](ctx context.Context, max int,
	fetch func(ctx context.Context, cursor wire.Cursor, limit int) ([]T, wire.Cursor, error),
) (items []T, truncated bool, err error) {
	want := -1 // no cap
	if max > 0 {
		want = max + 1
	}
	var cursor wire.Cursor
	for {
		limit := wire.MaxPageLimit
		if want > 0 && want-len(items) < limit {
			limit = want - len(items)
		}
		page, next, err := fetch(ctx, cursor, limit)
		if err != nil {
			return nil, false, err
		}
		items = append(items, page...)
		if want > 0 && len(items) >= want {
			return items[:max], true, nil
		}
		if next == "" {
			return items, false, nil
		}
		if next == cursor || len(page) == 0 {
			return nil, false, fmt.Errorf("apiclient: pagination cursor did not advance after %d items", len(items))
		}
		cursor = next
	}
}

// FactsAll returns every fact about subject, up to max (<= 0: no
// cap), following the /v1/facts cursor across pages. truncated is
// true when more than max facts exist.
func (c *Client) FactsAll(ctx context.Context, subject string, max int) ([]wire.SemanticFact, bool, error) {
	return collectPages(ctx, max, func(ctx context.Context, cursor wire.Cursor, limit int) ([]wire.SemanticFact, wire.Cursor, error) {
		resp, err := c.Facts(ctx, subject, limit, cursor)
		if err != nil {
			return nil, "", err
		}
		return resp.Facts, resp.NextCursor, nil
	})
}
