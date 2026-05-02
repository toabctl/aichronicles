package api

// SkillStaleness is the wire shape for one /v1/skills/staleness
// row. Mirrors store.SkillStaleness one-to-one (the store row is
// already JSON-tagged and JSON-clean — no nullable columns and
// no SQL-flavored types — so the projection is a straight copy).
type SkillStaleness struct {
	Name            string   `json:"name"`
	TotalLoads      int      `json:"total_loads"`
	StaleLoads      int      `json:"stale_loads"`
	Rate            float64  `json:"rate"`
	RateLowerBound  float64  `json:"rate_lower_bound"`
	AutoRefineScore float64  `json:"autorefine_score"`
	Examples        []string `json:"example_session_ids"`
}

// SkillStalenessRequest is the query-shape for
// GET /v1/skills/staleness.
type SkillStalenessRequest struct {
	SinceMs     int64 `json:"since_ms,omitempty"`
	WindowMs    int64 `json:"window_ms,omitempty"`
	MaxSkills   int   `json:"max_skills,omitempty"`
	MaxExamples int   `json:"max_examples,omitempty"`
}

// SkillStalenessResponse is the body for /v1/skills/staleness.
type SkillStalenessResponse struct {
	Skills []SkillStaleness `json:"skills"`
}
