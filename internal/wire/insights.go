package wire

// Insights is the wire shape for /v1/insights. Mirrors
// store.InsightsReport one-to-one (the store row is already
// JSON-tagged).
type Insights struct {
	Window         InsightsWindow   `json:"window"`
	Overview       InsightsOverview `json:"overview"`
	TopTools       []ToolUsage      `json:"top_tools"`
	TopSkills      []SkillUsage     `json:"top_skills"`
	ActivityByHour []HourBucket     `json:"activity_by_hour"`
	TopSessions    []TopSession     `json:"top_sessions"`
}

type InsightsWindow struct {
	SinceMs int64 `json:"since_ms"`
	UntilMs int64 `json:"until_ms"`
	Days    int   `json:"days"`
}

type InsightsOverview struct {
	Sessions       int `json:"sessions"`
	Events         int `json:"events"`
	ToolUses       int `json:"tool_uses"`
	UserPrompts    int `json:"user_prompts"`
	DistinctTools  int `json:"distinct_tools"`
	DistinctSkills int `json:"distinct_skills"`
}

type ToolUsage struct {
	ToolName string `json:"tool_name"`
	Count    int    `json:"count"`
}

type SkillUsage struct {
	Name       string `json:"name"`
	Count      int    `json:"count"`
	LastUsedMs int64  `json:"last_used_ms"`
}

type HourBucket struct {
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

type TopSession struct {
	SessionID   string  `json:"session_id"`
	EventCount  int     `json:"event_count"`
	StartedAtMs *int64  `json:"started_at_ms,omitempty"`
	EndedAtMs   *int64  `json:"ended_at_ms,omitempty"`
	Cwd         *string `json:"cwd,omitempty"`
	FirstPrompt string  `json:"first_prompt"`
}
