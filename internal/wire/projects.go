package wire

// ProjectAggregate is the wire shape for one /v1/projects/aggregates
// row. Mirrors store.ProjectAggregate — already wire-clean (no
// nullable columns).
type ProjectAggregate struct {
	Cwd            string `json:"cwd"`
	Sessions       int    `json:"sessions"`
	Events         int    `json:"events"`
	LastActivityMs int64  `json:"last_activity_ms"`
}

// ProjectAggregatesResponse is the body for /v1/projects/aggregates.
type ProjectAggregatesResponse struct {
	Projects []ProjectAggregate `json:"projects"`
}
