package api

// AuditRequest is the query-shape for GET /v1/audit. Both fields
// are optional; zero means "no filter".
type AuditRequest struct {
	SinceMs int64 `json:"since_ms,omitempty"`
	Limit   int   `json:"limit,omitempty"`
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
// over the scanned set so callers can render a summary without
// re-scanning.
type AuditResponse struct {
	Findings      []AuditFinding `json:"findings"`
	Scanned       int            `json:"scanned"`
	Flagged       int            `json:"flagged"`
	TotalFindings int            `json:"total_findings"`
	PatternHits   map[string]int `json:"pattern_hits"`
}
