package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/events/extract"
)

// InsightsReport is the full output of LoadInsights — the data the
// `aichronicles insights` CLI prints, and the JSON body callers
// (web, future MCP tool) can render however they like. Pure SQL
// aggregation; no LLM calls. Mirrors hermes-agent's
// agent/insights.py InsightsEngine output shape, but with our
// existing Claude-Code-flavoured data sources (events,
// extractions, sessions) rather than their tool_calls JSON
// projection.
type InsightsReport struct {
	Window         InsightsWindow   `json:"window"`
	Overview       InsightsOverview `json:"overview"`
	TopTools       []ToolUsage      `json:"top_tools"`
	TopSkills      []SkillUsage     `json:"top_skills"`
	ActivityByHour []HourBucket     `json:"activity_by_hour"` // 24 entries, hour 0-23 UTC
	TopSessions    []TopSession     `json:"top_sessions"`
}

// InsightsWindow describes the time range the report covers, both
// the half-open interval [SinceMs, UntilMs) and the rounded
// human-friendly day count for display.
type InsightsWindow struct {
	SinceMs int64 `json:"since_ms"`
	UntilMs int64 `json:"until_ms"`
	Days    int   `json:"days"`
}

// InsightsOverview is the headline counters block. DistinctTools
// counts unique tool_name values on tool_use events; DistinctSkills
// counts unique skill_load extraction values — which is strictly a
// subset of distinct skills the user has installed.
type InsightsOverview struct {
	Sessions       int `json:"sessions"`
	Events         int `json:"events"`
	ToolUses       int `json:"tool_uses"`
	UserPrompts    int `json:"user_prompts"`
	DistinctTools  int `json:"distinct_tools"`
	DistinctSkills int `json:"distinct_skills"`
}

// ToolUsage is one row of the "top tools" table. Count is the
// number of tool_use events with this tool_name in the window.
type ToolUsage struct {
	ToolName string `json:"tool_name"`
	Count    int    `json:"count"`
}

// SkillUsage is one row of the "top skills" table — the canonical
// skill identifier (kebab-case directory name) and how many times
// it was loaded in the window.
type SkillUsage struct {
	Name       string `json:"name"`
	Count      int    `json:"count"`
	LastUsedMs int64  `json:"last_used_ms"`
}

// HourBucket counts events at a given UTC hour-of-day across all
// days in the window. Useful as a "when does this user actually
// work" signal.
type HourBucket struct {
	Hour  int `json:"hour"` // 0-23 UTC
	Count int `json:"count"`
}

// TopSession is the per-session row of the "notable sessions"
// table, sorted by EventCount descending. We keep StartedAtMs,
// EndedAtMs, Cwd, and FirstPrompt so the renderer can show a
// short, recognisable line per session.
type TopSession struct {
	SessionID   string         `json:"session_id"`
	EventCount  int            `json:"event_count"`
	StartedAtMs sql.NullInt64  `json:"started_at_ms"`
	EndedAtMs   sql.NullInt64  `json:"ended_at_ms"`
	Cwd         sql.NullString `json:"cwd"`
	FirstPrompt string         `json:"first_prompt"`
}

// InsightsLimits caps how many rows each "top" table holds before
// an ellipsis. Sensible defaults; the caller can override per-call
// from CLI flags. Zero means "use the default."
type InsightsLimits struct {
	TopTools    int
	TopSkills   int
	TopSessions int
}

const (
	defaultTopTools    = 15
	defaultTopSkills   = 10
	defaultTopSessions = 10
)

// LoadInsights runs every aggregate query that feeds the report
// in a single call. ~6 queries; all read-only; cheap on indexed
// schemas. Empty report (zero sessions, zero events, …) is
// returned when nothing in the window matches — callers detect
// that and print "no sessions in window" rather than empty
// tables.
func LoadInsights(ctx context.Context, db *sql.DB, sinceMs int64, lim InsightsLimits) (*InsightsReport, error) {
	if lim.TopTools <= 0 {
		lim.TopTools = defaultTopTools
	}
	if lim.TopSkills <= 0 {
		lim.TopSkills = defaultTopSkills
	}
	if lim.TopSessions <= 0 {
		lim.TopSessions = defaultTopSessions
	}
	now := time.Now().UTC().UnixMilli()
	report := &InsightsReport{
		Window: InsightsWindow{
			SinceMs: sinceMs,
			UntilMs: now,
			Days:    int((now - sinceMs) / (24 * 60 * 60 * 1000)),
		},
	}

	overview, err := loadInsightsOverview(ctx, db, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("overview: %w", err)
	}
	report.Overview = overview

	tools, err := loadTopTools(ctx, db, sinceMs, lim.TopTools)
	if err != nil {
		return nil, fmt.Errorf("top tools: %w", err)
	}
	report.TopTools = tools

	skills, err := loadTopSkills(ctx, db, sinceMs, lim.TopSkills)
	if err != nil {
		return nil, fmt.Errorf("top skills: %w", err)
	}
	report.TopSkills = skills

	report.ActivityByHour, err = loadActivityByHour(ctx, db, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("activity by hour: %w", err)
	}

	report.TopSessions, err = loadTopSessions(ctx, db, sinceMs, lim.TopSessions)
	if err != nil {
		return nil, fmt.Errorf("top sessions: %w", err)
	}

	return report, nil
}

func loadInsightsOverview(ctx context.Context, db *sql.DB, sinceMs int64) (InsightsOverview, error) {
	// Six independent scalar counts, one query each. The previous
	// implementation packed them all into a single SELECT with six
	// uncorrelated subqueries; correct but unhelpful when one
	// subquery's plan went off — the wrapping QueryRow surfaced
	// only the combined error and the slow plan was hidden inside.
	// Splitting localises failures and lets each query use its
	// natural index without packed-query optimiser quirks.
	var o InsightsOverview
	type scalar struct {
		dst   *int
		query string
		args  []any
	}
	scalars := []scalar{
		{
			dst:   &o.Sessions,
			query: `SELECT COUNT(*) FROM sessions WHERE COALESCE(ended_at_ms, started_at_ms) >= ?`,
			args:  []any{sinceMs},
		},
		{
			dst:   &o.Events,
			query: `SELECT COUNT(*) FROM events WHERE ts_source_ms >= ?`,
			args:  []any{sinceMs},
		},
		{
			dst:   &o.ToolUses,
			query: `SELECT COUNT(*) FROM events WHERE ts_source_ms >= ? AND kind = ?`,
			args:  []any{sinceMs, events.KindToolUse},
		},
		{
			dst:   &o.UserPrompts,
			query: `SELECT COUNT(*) FROM events WHERE ts_source_ms >= ? AND kind = ?`,
			args:  []any{sinceMs, events.KindUserPrompt},
		},
		{
			dst:   &o.DistinctTools,
			query: `SELECT COUNT(DISTINCT tool_name) FROM events WHERE ts_source_ms >= ? AND kind = ? AND tool_name IS NOT NULL`,
			args:  []any{sinceMs, events.KindToolUse},
		},
		{
			dst: &o.DistinctSkills,
			query: `SELECT COUNT(DISTINCT value) FROM extractions WHERE kind = ?
				    AND session_id IN (SELECT id FROM sessions WHERE COALESCE(ended_at_ms, started_at_ms) >= ?)`,
			args: []any{extract.KindSkillLoad, sinceMs},
		},
	}
	for i, s := range scalars {
		if err := db.QueryRowContext(ctx, s.query, s.args...).Scan(s.dst); err != nil {
			return InsightsOverview{}, fmt.Errorf("overview scalar #%d: %w", i, err)
		}
	}
	return o, nil
}

func loadTopTools(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]ToolUsage, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT tool_name, COUNT(*) AS c
		   FROM events
		  WHERE ts_source_ms >= ? AND kind=? AND tool_name IS NOT NULL
		  GROUP BY tool_name
		  ORDER BY c DESC, tool_name ASC
		  LIMIT ?`,
		sinceMs, events.KindToolUse, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ToolUsage
	for rows.Next() {
		var t ToolUsage
		if err := rows.Scan(&t.ToolName, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func loadTopSkills(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]SkillUsage, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT x.value,
		        COUNT(*) AS c,
		        MAX(e.ts_source_ms) AS last_used_ms
		   FROM extractions x
		   JOIN events e ON e.event_id = x.event_id
		  WHERE x.kind = ? AND e.ts_source_ms >= ?
		  GROUP BY x.value
		  ORDER BY c DESC, x.value ASC
		  LIMIT ?`,
		extract.KindSkillLoad, sinceMs, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SkillUsage
	for rows.Next() {
		var s SkillUsage
		if err := rows.Scan(&s.Name, &s.Count, &s.LastUsedMs); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadActivityByHour returns 24 buckets keyed by UTC hour-of-day.
// Always 24 entries, even hours with zero events — the histogram
// renderer expects a dense array. Per-hour count = events whose
// ts_source_ms (converted to UTC hour) falls into that bucket.
func loadActivityByHour(ctx context.Context, db *sql.DB, sinceMs int64) ([]HourBucket, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT CAST(strftime('%H', ts_source_ms/1000, 'unixepoch') AS INTEGER) AS hour,
		        COUNT(*) AS c
		   FROM events
		  WHERE ts_source_ms >= ?
		  GROUP BY hour`,
		sinceMs,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]HourBucket, 24)
	for i := range out {
		out[i].Hour = i
	}
	for rows.Next() {
		var hour, count int
		if err := rows.Scan(&hour, &count); err != nil {
			return nil, err
		}
		if hour >= 0 && hour < 24 {
			out[hour].Count = count
		}
	}
	return out, rows.Err()
}

func loadTopSessions(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]TopSession, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT s.id, s.event_count, s.started_at_ms, s.ended_at_ms, s.cwd,
		        COALESCE(s.first_prompt_text, '') AS first_prompt
		   FROM sessions s
		  WHERE `+EffectiveTsExpr+` >= ?
		  ORDER BY s.event_count DESC, s.id ASC
		  LIMIT ?`,
		sinceMs, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TopSession
	for rows.Next() {
		var t TopSession
		if err := rows.Scan(&t.SessionID, &t.EventCount, &t.StartedAtMs, &t.EndedAtMs, &t.Cwd, &t.FirstPrompt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
