package api

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
	Limit             int    `json:"limit,omitempty"`
}

// SearchResponse is the body shape for GET /v1/search.
type SearchResponse struct {
	Hits []SearchHit `json:"hits"`
}
