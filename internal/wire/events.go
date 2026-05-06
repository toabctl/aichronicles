package wire

// Event is the wire shape for a single stored event as returned
// by /v1/events. Maps from the internal store's LiveEvent type
// at the handler boundary so the wire stays decoupled from the
// SQLite-flavored row shape.
//
// Nullable text columns use *string (not internal/events.NullString)
// so JSON encoding is plain "field": "value" or "field": null,
// and clients in any language can decode it without bespoke
// helpers.
type Event struct {
	IngestSeq  int64   `json:"ingest_seq"`
	EventID    string  `json:"event_id"`
	SessionID  string  `json:"session_id"`
	Kind       string  `json:"kind"`
	TsSourceMs int64   `json:"ts_source_ms"`
	TsServerMs int64   `json:"ts_server_ms"`
	Cwd        *string `json:"cwd,omitempty"`
	Snippet    *string `json:"snippet,omitempty"`
}

// EventListRequest is the query-shape for GET /v1/events.
//
// SinceSeq is an exclusive cursor on the monotonically-increasing
// `ingest_seq` column. Clients page forward by passing the highest
// IngestSeq they have seen; an empty SinceSeq means "from the
// beginning."
//
// Limit honors the same DefaultPageLimit / MaxPageLimit semantics
// as PageRequest, but events use a typed cursor (int64 ingest_seq)
// rather than the opaque pagination cursor — clients of /v1/events
// already understand the watermark and use it for SSE catch-up.
type EventListRequest struct {
	SessionID string `json:"session_id,omitempty"`
	SinceSeq  int64  `json:"since_seq,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// EventListResponse is the body shape for GET /v1/events.
// LatestSeq is the maximum ingest_seq currently in the store —
// useful for clients that want to know whether their fetch is
// caught up without doing a separate query.
type EventListResponse struct {
	Events    []Event `json:"events"`
	LatestSeq int64   `json:"latest_seq"`
}
