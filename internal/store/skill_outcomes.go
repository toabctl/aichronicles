package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// SkillImpact is one row of a skill-impact aggregation: how often
// the skill was loaded and what fraction of those loads were
// followed by a tool_failure event in the same session within the
// configured window. Companion to SkillStaleness — staleness
// returns only the bad skills (HAVING stale_loads > 0); impact
// covers EVERY loaded skill so the user can see the full picture
// (the high-success ones too) and `propose` can hand the model a
// success-rate-aware view of installed skills.
type SkillImpact struct {
	Name         string
	TotalLoads   int
	FailedLoads  int
	SuccessRate  float64 // (TotalLoads - FailedLoads) / TotalLoads, 0..1
	LastLoadedMs int64   // ts_source_ms of the most recent load in the window
}

// SkillImpactLimits caps the result-set size of LoadSkillImpact.
// Zero / negative values fall back to defaultMaxImpactSkills.
type SkillImpactLimits struct {
	MaxSkills int
}

const defaultMaxImpactSkills = 100

// LoadSkillImpact returns one SkillImpact row per skill that was
// loaded at least once in the window. Counts loads followed by a
// same-session tool_failure within `windowMs`, computes a positive
// success rate (rather than the negative stale rate
// LoadSkillStaleness exposes), and keeps every skill — including
// the 100%-success ones — so callers like `propose` can read the
// full distribution.
//
// Mirrors LoadSkillStaleness's correlated-subquery shape so
// idx_events_session_ts handles the per-load failure probe at
// indexed-lookup speed.
//
// windowMs <= 0 → defaultStalenessWindow (10 min, same as the
// staleness detector — keep them in sync so the two views agree
// on what "this load failed" means).
func LoadSkillImpact(ctx context.Context, db *sql.DB, sinceMs int64, windowMs int64, lim SkillImpactLimits) ([]SkillImpact, error) {
	if windowMs <= 0 {
		windowMs = defaultStalenessWindow
	}
	if lim.MaxSkills <= 0 {
		lim.MaxSkills = defaultMaxImpactSkills
	}

	// Deliberately no HAVING clause: success-rate-aware callers
	// want the full set, not just the trouble ones. ORDER puts
	// most-loaded first so the propose prompt's leading rows are
	// the high-signal skills.
	const aggQuery = `
WITH loads AS (
    SELECT x.value          AS skill,
           x.session_id      AS session_id,
           e.ts_source_ms    AS ts
      FROM extractions x
      JOIN events e ON e.event_id = x.event_id
     WHERE x.kind = ? AND e.ts_source_ms >= ?
)
SELECT skill,
       COUNT(*)                                                   AS total_loads,
       SUM(CASE WHEN EXISTS (
           SELECT 1
             FROM events f
            WHERE f.session_id = loads.session_id
              AND f.kind       = 'tool_failure'
              AND f.ts_source_ms >  loads.ts
              AND f.ts_source_ms <= loads.ts + ?
       ) THEN 1 ELSE 0 END)                                        AS failed_loads,
       MAX(loads.ts)                                              AS last_loaded_ms
  FROM loads
 GROUP BY skill
 ORDER BY total_loads DESC, skill ASC
 LIMIT ?`

	rows, err := db.QueryContext(ctx, aggQuery,
		extract.KindSkillLoad, sinceMs, windowMs, lim.MaxSkills,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate skill impact: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SkillImpact
	for rows.Next() {
		var s SkillImpact
		if err := rows.Scan(&s.Name, &s.TotalLoads, &s.FailedLoads, &s.LastLoadedMs); err != nil {
			return nil, fmt.Errorf("scan skill impact: %w", err)
		}
		if s.TotalLoads > 0 {
			s.SuccessRate = float64(s.TotalLoads-s.FailedLoads) / float64(s.TotalLoads)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
