package api

// ImportStats is the response body for POST /v1/import. Mirrors
// the per-line outcomes the bulk importer needs to surface to
// the operator: how many lines were read, how many produced a new
// row, how many were duplicates of an existing event_id, and how
// many were rejected (malformed JSON, validation failure).
type ImportStats struct {
	LinesRead int   `json:"lines_read"`
	Imported  int   `json:"imported"`
	Deduped   int   `json:"deduped"`
	Invalid   int   `json:"invalid"`
	DurationM int64 `json:"duration_ms"`
}
