package api

// UsageRow is the wire shape for one (day, kind, model) bucket of
// LLM-token spend returned by /v1/usage. Day is a UTC YYYY-MM-DD
// string so jq pipelines can sort lexicographically and the
// renderer doesn't have to reformat. Mirrors store.TokenUsageRow.
type UsageRow struct {
	Day          string `json:"day"`
	Kind         string `json:"kind"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	RowCount     int    `json:"row_count"`
}

// UsageTotals is the headline summary across a UsageRow slice.
// Computed server-side and shipped on the response so jq can grab
// `.totals.input_tokens` without re-summing.
type UsageTotals struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	RowCount     int   `json:"row_count"`
}

// UsageRequest is the query-shape for GET /v1/usage. Zero
// since_ms means "all time".
type UsageRequest struct {
	SinceMs int64 `json:"since_ms,omitempty"`
}

// UsageResponse is the body for /v1/usage. Cost is intentionally
// not on the wire — pricing is a client-side concern (the api
// doesn't ship a price list and shouldn't carry one).
type UsageResponse struct {
	Rows   []UsageRow  `json:"rows"`
	Totals UsageTotals `json:"totals"`
}
