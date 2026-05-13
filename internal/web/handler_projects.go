package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/wire"
)

// projectsDefaultDays is the window /projects covers by default.
// Mirrors /skills and /insights so all three feel like the same
// "what have I been doing lately" surface with the same axis.
const projectsDefaultDays = 30

// projectsHandler renders /projects: each project root the user
// has worked in within the window, with session/event counts and
// last-activity timestamps. Project roots are derived by walking
// up each session's start cwd to the nearest .claude / .git /
// go.mod ancestor — sessions inside the same repo collapse into
// one row instead of producing N rows per subdirectory.
func (s *Server) projectsHandler(w http.ResponseWriter, r *http.Request) {
	days := projectsDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	resp, err := s.api.ProjectAggregates(r.Context(), sinceMs)
	if err != nil {
		s.log.Error("projectsHandler: load", "err", err)
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}

	page := buildProjectsPage(resp.Projects, days, time.Now().UTC())
	s.render(w, r, "projects", page)
}

// buildProjectsPage rolls the per-cwd aggregates up to project
// roots: walk each cwd up to the nearest .claude/.git/go.mod
// ancestor, then sum sessions+events into the bucket keyed by
// that ancestor.
func buildProjectsPage(aggs []wire.ProjectAggregate, days int, now time.Time) ProjectsPage {
	type bucket struct {
		Sessions       int
		Events         int
		LastActivityMs int64
		Cwds           map[string]struct{} // distinct start cwds rolled up here
	}
	byRoot := make(map[string]*bucket, len(aggs))
	for _, a := range aggs {
		root := skills.FindProjectRootGeneric(a.Cwd)
		b, ok := byRoot[root]
		if !ok {
			b = &bucket{Cwds: make(map[string]struct{})}
			byRoot[root] = b
		}
		b.Sessions += a.Sessions
		b.Events += a.Events
		if a.LastActivityMs > b.LastActivityMs {
			b.LastActivityMs = a.LastActivityMs
		}
		b.Cwds[a.Cwd] = struct{}{}
	}

	rows := make([]ProjectRow, 0, len(byRoot))
	for root, b := range byRoot {
		rows = append(rows, ProjectRow{
			Root:         root,
			Sessions:     b.Sessions,
			Events:       b.Events,
			LastActivity: relativeTime(b.LastActivityMs, now),
			SortKey:      b.LastActivityMs,
			DistinctCwds: len(b.Cwds),
		})
	}
	// Newest activity first; ties broken by path so output is
	// deterministic across runs.
	sortProjectRowsByActivity(rows)

	return ProjectsPage{
		Title:    "Projects",
		Days:     days,
		Empty:    len(rows) == 0,
		Projects: rows,
	}
}

// sortProjectRowsByActivity is an in-place stable sort by descending
// LastActivityMs, then ascending Root path.
func sortProjectRowsByActivity(rows []ProjectRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := rows[j-1], rows[j]
			swap := a.SortKey < b.SortKey || (a.SortKey == b.SortKey && a.Root > b.Root)
			if !swap {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}
