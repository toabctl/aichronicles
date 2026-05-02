package api

// UnresolvedItem is the wire shape for one /v1/unresolved row.
// Mirrors store.UnresolvedItem.
type UnresolvedItem struct {
	SessionID    string `json:"session_id"`
	SessionShort string `json:"session_short"`
	EndedAtMs    int64  `json:"ended_at_ms"` // 0 when session is still active
	Topic        string `json:"topic"`
	Item         string `json:"item"`
}

// UnresolvedResponse is the body for /v1/unresolved.
type UnresolvedResponse struct {
	Items []UnresolvedItem `json:"items"`
}
