package wire

// StreamEvent is the wire shape of one SSE frame from
// GET /v1/stream. Carries enough info for a client to render a
// "new activity" line and decide whether to fetch full details
// (the consumer can call /v1/events/{event_id} for the rest).
//
// Encoded as JSON in the SSE `data:` line. Reserve room for
// future fields: clients SHOULD ignore unknown ones.
type StreamEvent struct {
	IngestSeq  int64  `json:"ingest_seq"`
	EventID    string `json:"event_id"`
	SessionID  string `json:"session_id"`
	Kind       string `json:"kind"`
	TsServerMs int64  `json:"ts_server_ms"`
}
