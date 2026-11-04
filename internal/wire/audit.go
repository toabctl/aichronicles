package wire

// AuditRequest is the query-shape for GET /v1/audit. Every field is
// optional.
//
// Rows are scanned most-recently-ingested first. Limit is the page
// size; zero (or anything above the server's per-call ceiling) means
// the ceiling. A full scan follows NextCursor until it comes back
// empty, passing it as Cursor; SinceMs (a ts_source_ms lower bound)
// must be re-sent unchanged on every page.
type AuditRequest struct {
	SinceMs int64  `json:"since_ms,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Cursor  Cursor `json:"cursor,omitempty"`
}

// AuditFinding is the wire shape for one flagged event row.
// Snippet is always rendered with the matched secret replaced by
// the canonical <pattern> marker — the raw bytes never traverse
// the wire.
type AuditFinding struct {
	SessionID  string   `json:"session_id"`
	TsSourceMs *int64   `json:"ts_source_ms,omitempty"`
	Kind       string   `json:"kind"`
	Patterns   []string `json:"patterns"`
	Snippet    string   `json:"snippet"`
}

// AuditResponse is the body for /v1/audit. Counts are aggregates
// over this page's scanned rows; a caller paging through the whole
// table sums them. An empty NextCursor means the scan is complete —
// anything else means rows remain unscanned.
type AuditResponse struct {
	PageResponse

	Findings      []AuditFinding `json:"findings"`
	Scanned       int            `json:"scanned"`
	Flagged       int            `json:"flagged"`
	TotalFindings int            `json:"total_findings"`
	PatternHits   map[string]int `json:"pattern_hits"`
}
