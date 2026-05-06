package wire

// Cursor is an opaque page token. The server treats it as a black
// box and decodes it server-side; clients pass it back verbatim
// from a previous response's NextCursor. An empty Cursor means
// "first page."
//
// Wire shape: a base64-url-no-padding string. Decoded contents
// are server-internal — do not rely on the format.
type Cursor string

// PageRequest is the embedded request fragment for any list
// endpoint that paginates. Limit defaults to 50 and is clamped
// server-side to MaxPageLimit; clients SHOULD send Limit=0 to
// accept the default rather than guessing.
type PageRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor Cursor `json:"cursor,omitempty"`
}

// PageResponse is the embedded response fragment for any list
// endpoint that paginates. NextCursor is empty when the server
// has no more pages — clients use that signal to stop fetching,
// not the length of Items.
type PageResponse struct {
	NextCursor Cursor `json:"next_cursor,omitempty"`
}

// MaxPageLimit caps server-side fan-out per request. A client
// asking for more than this gets MaxPageLimit rows back; the
// response always honors the cursor contract so over-asking is
// safe (no error, just clamped).
const MaxPageLimit = 1000

// DefaultPageLimit is the value the server applies when a client
// sends Limit=0 (or omits the field). Conservative — endpoints
// that want a different default override at handler time.
const DefaultPageLimit = 50
