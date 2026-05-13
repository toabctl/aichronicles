package wire

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

// SkillImpact is the wire shape for one /v1/skills/impact row.
// Sister of SkillStaleness — covers every loaded skill (including
// the 100%-success ones) so callers can render the full
// distribution rather than just the trouble subset.
type SkillImpact struct {
	Name         string  `json:"name"`
	TotalLoads   int     `json:"total_loads"`
	FailedLoads  int     `json:"failed_loads"`
	SuccessRate  float64 `json:"success_rate"`
	LastLoadedMs int64   `json:"last_loaded_ms"`
}

// SkillImpactRequest is the query-shape for GET /v1/skills/impact.
type SkillImpactRequest struct {
	SinceMs   int64 `json:"since_ms,omitempty"`
	WindowMs  int64 `json:"window_ms,omitempty"`
	MaxSkills int   `json:"max_skills,omitempty"`
}

// SkillImpactResponse is the body for /v1/skills/impact.
type SkillImpactResponse struct {
	Skills []SkillImpact `json:"skills"`
}

// InvokedSkill is the wire shape for one /v1/skills/invoked row:
// a skill name and how many times it was loaded in the window.
// Mirrors prompts.InvokedSkill so callers don't need a separate
// projection on the consumer side.
type InvokedSkill struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// InvokedSkillsRequest is the query-shape for GET /v1/skills/invoked.
type InvokedSkillsRequest struct {
	SinceMs int64 `json:"since_ms,omitempty"`
}

// InvokedSkillsResponse is the body for /v1/skills/invoked.
type InvokedSkillsResponse struct {
	Skills []InvokedSkill `json:"skills"`
}

// InstalledSkill is the wire shape for one /v1/skills/installed row:
// a SKILL.md file the daemon discovered on disk (global ~/.claude/skills
// or project-local under any observed session cwd). Mirrors
// prompts.InstalledSkill 1:1 so the handler projection is a direct
// copy.
type InstalledSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is the discovery scope: "global" for ~/.claude/skills,
	// "project:<abs-path>" for project-local installs, "plugin:<id>"
	// for plugin-provided. Renderers group rows by this prefix.
	Source string `json:"source"`
}

// InstalledSkillsRequest is the query-shape for
// GET /v1/skills/installed. SinceMs scopes which session cwds the
// daemon walks for project-local discovery (older sessions are
// ignored so a long-running install doesn't pile up dead roots).
type InstalledSkillsRequest struct {
	SinceMs int64 `json:"since_ms,omitempty"`
}

// InstalledSkillsResponse is the body for /v1/skills/installed.
type InstalledSkillsResponse struct {
	Skills []InstalledSkill `json:"skills"`
}
