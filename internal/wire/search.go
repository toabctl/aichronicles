package wire

import (
	"fmt"
	"hash/fnv"
	"strconv"
)

// SearchHit is the wire shape for a single search result row from
// /v1/search. Snippet is the FTS5-computed match-centered excerpt;
// Content is the full original event content_text. Both are
// nullable on the wire because not every event kind populates
// content_text.
type SearchHit struct {
	SessionID  string  `json:"session_id"`
	Kind       string  `json:"kind"`
	Cwd        *string `json:"cwd,omitempty"`
	TsSourceMs int64   `json:"ts_source_ms"`
	Content    *string `json:"content,omitempty"`
	Snippet    *string `json:"snippet,omitempty"`
}

// SearchRequest is the query-shape for GET /v1/search.
//
// Q is required; the remaining fields are optional filters AND'd
// together. The server side parses Q via internal/searchquery so
// callers can use the same FTS5 syntax as the CLI / web search:
// `"exact phrase"`, `term1 OR term2`, `path:foo` etc.
//
// Limit defaults to DefaultPageLimit, capped at MaxPageLimit.
type SearchRequest struct {
	Q                 string `json:"q"`
	Kind              string `json:"kind,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	SubagentID        string `json:"subagent_id,omitempty"`
	SourceAgent       string `json:"source_agent,omitempty"`
	ToolName          string `json:"tool_name,omitempty"`
	SkillName         string `json:"skill_name,omitempty"`
	FilePathSubstring string `json:"file_path_substring,omitempty"`
	SinceMs           int64  `json:"since_ms,omitempty"`
	WithFailures      bool   `json:"with_failures,omitempty"`
	// NoDedup disables the default same-turn collapsing that picks
	// transport=hook over a transcript-import duplicate. Set true
	// only when the caller wants to see every captured row, e.g.
	// to debug ingest fan-out.
	NoDedup bool `json:"no_dedup,omitempty"`
	// Order is SearchOrderRank (the default when empty) or
	// SearchOrderRecency; anything else is a 400. Like NoDedup it is
	// pinned by the cursor, which wins over a re-sent value.
	Order string `json:"order,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// Cursor pages forward through a previous response's NextCursor.
	// Empty means "first page." Pass it back verbatim with the SAME q
	// and filters: the cursor carries only the page position, the
	// locked stage / as-of snapshot and a fingerprint of q + filters,
	// and a mismatch is a 400. See SearchCursor and the as-of
	// semantics on SearchResponse.
	Cursor Cursor `json:"cursor,omitempty"`
}

// Search orders accepted by GET /v1/search?order=.
const (
	// SearchOrderRank sorts by recency-boosted FTS relevance.
	SearchOrderRank = "rank"
	// SearchOrderRecency sorts newest first, ignoring relevance.
	SearchOrderRecency = "recency"
)

// QueryFingerprint hashes the fields that define a search's result
// set — q and every filter — for SearchCursor.Query. Paging controls
// (Limit, Cursor) are excluded, and so are NoDedup and the order,
// which the cursor pins itself. Fields are length-prefixed so no two
// different requests share an encoding.
func (r SearchRequest) QueryFingerprint() uint64 {
	h := fnv.New64a()
	for _, v := range []string{
		r.Q, r.Kind, r.SessionID, r.SubagentID, r.SourceAgent,
		r.ToolName, r.SkillName, r.FilePathSubstring,
		strconv.FormatInt(r.SinceMs, 10), strconv.FormatBool(r.WithFailures),
	} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(v), v)
	}
	return h.Sum64()
}

// SearchResponse is the body shape for GET /v1/search.
//
// Pagination uses an offset cursor that pins the relevance as-of
// timestamp and the FTS fallback stage, so the row ORDER is identical
// on every page and pages never mix result corpora. As with any
// offset pagination, rows ingested or removed between page fetches
// can shift the window (a boundary row may repeat or be missed) —
// fine for an interactive refine-as-you-go search, not a
// consistent-snapshot bulk export. NextCursor is empty when there are
// no more pages; clients stop on that signal, not on len(Hits).
type SearchResponse struct {
	Hits       []SearchHit `json:"hits"`
	NextCursor Cursor      `json:"next_cursor,omitempty"`
}
