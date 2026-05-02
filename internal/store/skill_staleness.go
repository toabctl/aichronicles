package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/events/extract"
)

// SkillStaleness is the per-skill staleness summary produced by
// LoadSkillStaleness. A skill is "stale-correlated" if loading it
// is followed by a tool_failure event in the same session within
// a short window — a signal that the skill's instructions led the
// agent into a dead end.
//
// Rate is StaleLoads / TotalLoads as a fraction in [0,1]. RateLowerBound
// is the Wilson-score 95%-confidence lower bound on Rate, useful as a
// sample-size-aware filter: a 1/1 skill has Rate=1.0 but a low Wilson
// bound (~0.21), while a 50/100 skill has Rate=0.5 with a much higher
// bound (~0.40). Threshold checks ("only revise skills above 50%
// stale") should prefer RateLowerBound to keep noise from low-N skills
// out of the work queue. Example session ids (max 3) help the user
// click through to a concrete instance and decide whether to revise.
//
// AutoRefineScore is the AutoRefine (Qiu et al., 2026 —
// arXiv:2601.22758) score combining effectiveness, frequency, and
// precision into one ranking signal:
//
//	score = (s/(u+ε)) · log(1+u) · (1 + u/(r+ε))
//
// where s = effectiveness (success rate, 1 - Rate), u = utilised
// (successful loads = TotalLoads - StaleLoads), r = retrieved
// (TotalLoads), ε = 1.0. Higher score = more frequently used and
// more reliable; LOWER score in the staleness view = stronger
// retire candidate. Distinct from RateLowerBound (which is purely
// confidence-on-failure-rate): AutoRefineScore also penalises
// skills nobody loads, even when their failure rate is statistically
// low. The two signals disagree most usefully on niche-but-broken
// skills (high Wilson, irrelevant AutoRefine) vs popular-and-buggy
// skills (low Wilson, low AutoRefine).
type SkillStaleness struct {
	Name            string   `json:"name"`
	TotalLoads      int      `json:"total_loads"`
	StaleLoads      int      `json:"stale_loads"`
	Rate            float64  `json:"rate"`
	RateLowerBound  float64  `json:"rate_lower_bound"`
	AutoRefineScore float64  `json:"autorefine_score"`
	Examples        []string `json:"example_session_ids"`
}

// autoRefineScore implements the AutoRefine (Qiu et al., 2026 —
// arXiv:2601.22758) compound ranking score:
//
//	(s/(u+ε)) · log(1+u) · (1 + u/(r+ε))
//
// where s ∈ [0,1] is effectiveness (success rate), u is the count
// of utilised loads (successful), r is the count of retrieved
// loads (total). ε = 1.0 prevents divide-by-zero on never-loaded
// skills. Returns 0 when r == 0 (no data — can't score).
//
// The 4.5×/8.9× repo-bloat / utilisation-drop the paper reports
// without active maintenance directly motivates having a
// utilisation-aware signal alongside Wilson — failure rate alone
// retires niche skills that simply have nothing to be wrong about,
// while AutoRefine retires skills nobody calls regardless of their
// reliability.
func autoRefineScore(retrieved, stale int) float64 {
	if retrieved <= 0 {
		return 0
	}
	r := float64(retrieved)
	u := float64(retrieved - stale) // utilised = successful loads
	if u < 0 {
		u = 0
	}
	s := u / r // effectiveness in [0,1]
	const eps = 1.0
	return (s / (u + eps)) * math.Log(1+u) * (1 + u/(r+eps))
}

// wilsonLowerBound returns the Wilson-score 95%-CI lower bound on a
// success probability estimated from `successes` out of `total`
// observations. For total=0 it returns 0; for total=1 successes=1 it
// returns ~0.205, vs the naive rate of 1.0 — exactly the property
// that makes it the right ranking key for low-N stale skills.
//
// Reference: Wilson, E. B. (1927). "Probable inference, the law of
// succession, and statistical inference." JASA 22 (158): 209–212.
// We use z=1.96 for a 95% confidence interval, the standard pick.
func wilsonLowerBound(successes, total int) float64 {
	if total <= 0 {
		return 0
	}
	// Defensive clamp: successes outside [0, total] would push
	// phat*(1-phat) negative, NaN-poisoning the sqrt and bypassing
	// the `lb < 0` guard below (NaN comparisons are always false).
	// Currently unreachable from the SQL caller, but the function
	// is package-exported in spirit and a future caller shouldn't
	// learn this the hard way via NaN propagating into a sort key.
	if successes < 0 || successes > total {
		return 0
	}
	const z = 1.96
	n := float64(total)
	phat := float64(successes) / n
	denom := 1 + z*z/n
	center := phat + z*z/(2*n)
	half := z * math.Sqrt(phat*(1-phat)/n+z*z/(4*n*n))
	lb := (center - half) / denom
	if lb < 0 {
		return 0
	}
	return lb
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

	// Reuse the impact aggregator (same correlated-subquery shape)
	// rather than duplicating the SQL. We call it with the package
	// default cap (defaultMaxImpactSkills = 100) so the staleness
	// view sees the full distribution before HAVING-filtering down
	// to "skills with at least one failed load." For a personal-use
	// store with at most a few dozen distinct skills, the extra
	// rows are free.
	impact, err := LoadSkillImpact(ctx, db, sinceMs, windowMs, SkillImpactLimits{})
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}

	// Filter to "stale-correlated" rows + recompute the inverse
	// rate (stale / total instead of success / total).
	out := make([]SkillStaleness, 0, len(impact))
	for _, s := range impact {
		if s.FailedLoads == 0 {
			continue
		}
		row := SkillStaleness{
			Name:       s.Name,
			TotalLoads: s.TotalLoads,
			StaleLoads: s.FailedLoads,
		}
		row.Rate = float64(s.FailedLoads) / float64(s.TotalLoads)
		row.RateLowerBound = wilsonLowerBound(s.FailedLoads, s.TotalLoads)
		row.AutoRefineScore = autoRefineScore(s.TotalLoads, s.FailedLoads)
		out = append(out, row)
	}

	// Order most-likely-broken first: stale_loads DESC, total_loads
	// DESC, skill ASC. Stable; matches the previous SQL ORDER BY.
	sort.Slice(out, func(i, j int) bool {
		if out[i].StaleLoads != out[j].StaleLoads {
			return out[i].StaleLoads > out[j].StaleLoads
		}
		if out[i].TotalLoads != out[j].TotalLoads {
			return out[i].TotalLoads > out[j].TotalLoads
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > lim.MaxSkills {
		out = out[:lim.MaxSkills]
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
          AND f.kind       = ?
          AND f.ts_source_ms >  e.ts_source_ms
          AND f.ts_source_ms <= e.ts_source_ms + ?
   )
 ORDER BY x.session_id
 LIMIT ?`
	rows, err := db.QueryContext(ctx, q,
		extract.KindSkillLoad, skill, sinceMs,
		events.KindToolFailure, windowMs, limit,
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
