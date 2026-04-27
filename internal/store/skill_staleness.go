package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/pkg/ingest/extract"
)

// SkillStaleness is the per-skill staleness summary produced by
// LoadSkillStaleness. A skill is "stale-correlated" if loading it
// is followed by a tool_failure event in the same session within
// a short window — a signal that the skill's instructions led the
// agent into a dead end.
//
// Rate is StaleLoads / TotalLoads as a fraction in [0,1]. Example
// session ids (max 3) help the user click through to a concrete
// instance and decide whether to revise the skill.
type SkillStaleness struct {
	Name       string   `json:"name"`
	TotalLoads int      `json:"total_loads"`
	StaleLoads int      `json:"stale_loads"`
	Rate       float64  `json:"rate"`
	Examples   []string `json:"example_session_ids"`
}

// SkillStalenessLimits caps how many skills the report holds and
// how many example session ids per skill. Zero values use sane
// defaults — the caller passes a zero struct from CLI flags.
type SkillStalenessLimits struct {
	MaxSkills   int
	MaxExamples int
}

const (
	defaultMaxStaleSkills   = 20
	defaultMaxStaleExamples = 3
	defaultStalenessWindow  = int64(10 * 60 * 1000) // 10 minutes in ms
)

// LoadSkillStaleness scans every skill_load extraction in the
// window and counts, per skill, how many were followed by a
// tool_failure event in the same session within `windowMs` ms of
// the load. Returns rows sorted by stale-loads descending, then
// rate descending — most-likely-broken first.
//
// windowMs <= 0 → defaultStalenessWindow.
func LoadSkillStaleness(ctx context.Context, db *sql.DB, sinceMs int64, windowMs int64, lim SkillStalenessLimits) ([]SkillStaleness, error) {
	if windowMs <= 0 {
		windowMs = defaultStalenessWindow
	}
	if lim.MaxSkills <= 0 {
		lim.MaxSkills = defaultMaxStaleSkills
	}
	if lim.MaxExamples <= 0 {
		lim.MaxExamples = defaultMaxStaleExamples
	}

	// Per-skill (total, stale) counts. The correlated subquery is
	// run by SQLite's optimiser using idx_events_session_ts —
	// indexed lookup of "events in session X with ts in [a,b] and
	// kind=tool_failure", which is the key workload here.
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
       ) THEN 1 ELSE 0 END)                                        AS stale_loads
  FROM loads
 GROUP BY skill
HAVING stale_loads > 0
 ORDER BY stale_loads DESC, total_loads DESC, skill ASC
 LIMIT ?`

	rows, err := db.QueryContext(ctx, aggQuery,
		extract.KindSkillLoad, sinceMs, windowMs, lim.MaxSkills,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SkillStaleness
	for rows.Next() {
		var s SkillStaleness
		if err := rows.Scan(&s.Name, &s.TotalLoads, &s.StaleLoads); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if s.TotalLoads > 0 {
			s.Rate = float64(s.StaleLoads) / float64(s.TotalLoads)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Per-skill examples: one cheap query per row, capped at
	// MaxExamples. We do this in a second pass rather than as a
	// GROUP_CONCAT subquery in the aggregate because GROUP_CONCAT
	// can't easily be both deduplicated and bounded in a
	// portable-SQLite way.
	for i := range out {
		exs, err := loadStaleExamples(ctx, db, out[i].Name, sinceMs, windowMs, lim.MaxExamples)
		if err != nil {
			return nil, fmt.Errorf("examples for %q: %w", out[i].Name, err)
		}
		out[i].Examples = exs
	}
	return out, nil
}

// loadStaleExamples returns up to limit distinct session ids where
// the named skill was loaded and a tool_failure followed within
// windowMs.
func loadStaleExamples(ctx context.Context, db *sql.DB, skill string, sinceMs, windowMs int64, limit int) ([]string, error) {
	const q = `
SELECT DISTINCT x.session_id
  FROM extractions x
  JOIN events e ON e.event_id = x.event_id
 WHERE x.kind = ? AND x.value = ? AND e.ts_source_ms >= ?
   AND EXISTS (
       SELECT 1 FROM events f
        WHERE f.session_id = x.session_id
          AND f.kind       = 'tool_failure'
          AND f.ts_source_ms >  e.ts_source_ms
          AND f.ts_source_ms <= e.ts_source_ms + ?
   )
 ORDER BY x.session_id
 LIMIT ?`
	rows, err := db.QueryContext(ctx, q,
		extract.KindSkillLoad, skill, sinceMs, windowMs, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FormatStaleSummary renders a one-line summary suitable for log
// output. Used by the CLI verbose mode and tests.
func FormatStaleSummary(s SkillStaleness) string {
	short := s.Name
	if len(short) > 32 {
		short = short[:29] + "..."
	}
	pct := int(s.Rate * 100)
	return fmt.Sprintf("%-32s %4d / %4d  (%2d%%)  examples=[%s]",
		short, s.StaleLoads, s.TotalLoads, pct, strings.Join(s.Examples, ", "))
}
