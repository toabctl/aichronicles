package wire

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// SearchCursor is the decoded payload of a /v1/search pagination
// Cursor. It is opaque on the wire (base64url(json)); clients pass
// the Cursor back verbatim and never construct one themselves.
//
// The cursor locks everything that, if it drifted between pages,
// would silently reorder rows or mix result corpora:
//
//   - Stage pins which FTS fallback stage the first page selected
//     (primary → trigram → extractions). Later pages query ONLY that
//     stage, so a deep page that runs off the end of the locked stage
//     returns nothing rather than falling through to the next corpus.
//   - Now pins the as-of timestamp the recency-boosted relevance
//     order is computed against, so rows don't reorder as the clock
//     advances between page fetches.
//   - Ord / Dedup pin the order mode and dedup flag.
//
// q and the filters are NOT carried here — the client re-sends them
// alongside the cursor (same shape as PageRequest), and the server
// re-applies them each page.
type SearchCursor struct {
	Off   int    `json:"o"` // OFFSET for the next page
	Stage string `json:"s"` // locked FTS stage
	Now   int64  `json:"n"` // pinned now-ms (as-of snapshot)
	Ord   int    `json:"r"` // store.SearchOrder mode (0=rank, 1=recency)
	Dedup bool   `json:"d"` // locked NoDedup flag
}

// EncodeSearchCursor renders a SearchCursor as an opaque Cursor:
// base64url-no-padding over its JSON. Returns an error only if the
// payload can't be marshalled (it always can), so callers may treat
// it as infallible in practice but must still check.
func EncodeSearchCursor(c SearchCursor) (Cursor, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode search cursor: %w", err)
	}
	return Cursor(base64.RawURLEncoding.EncodeToString(b)), nil
}

// DecodeSearchCursor parses an opaque Cursor back into a
// SearchCursor. Returns an error for malformed input (bad base64 or
// bad JSON) so the handler can reply 400 rather than paging from a
// corrupt position.
func DecodeSearchCursor(c Cursor) (SearchCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return SearchCursor{}, fmt.Errorf("decode search cursor: %w", err)
	}
	var out SearchCursor
	if err := json.Unmarshal(raw, &out); err != nil {
		return SearchCursor{}, fmt.Errorf("decode search cursor: %w", err)
	}
	return out, nil
}
