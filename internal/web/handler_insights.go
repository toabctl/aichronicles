package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/wire"
)

// insightsDefaultDays matches `aichronicles insights`'s default
// window. Configurable per-request via ?days=<n>.
const insightsDefaultDays = 30

// insightsHandler renders /insights — the same overview /
// top-tools / top-skills / activity-by-hour / top-sessions data
// the CLI prints, as HTML. Pure SQL aggregation; no LLM call.
func (s *Server) insightsHandler(w http.ResponseWriter, r *http.Request) {
	days := insightsDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	report, err := s.api.Insights(r.Context(), apiclient.InsightsRequest{SinceMs: sinceMs})
	if err != nil {
		s.log.Error("insightsHandler: load", "err", err)
		http.Error(w, "could not load insights", http.StatusInternalServerError)
		return
	}

	page := buildInsightsPage(report, days)
	s.render(w, r, "insights", page)
}

// buildInsightsPage shapes the wire report into the rendering
// view: pre-formatted strings + bar widths for the activity
// histogram + percentages for top-tools. Done server-side so the
// template stays free of helpers.
func buildInsightsPage(r wire.Insights, days int) InsightsPage {
	now := time.Now()
	page := InsightsPage{
		Title:    "Insights",
		Days:     days,
		Since:    time.UnixMilli(r.Window.SinceMs).UTC().Format("2006-01-02"),
		Until:    time.UnixMilli(r.Window.UntilMs).UTC().Format("2006-01-02"),
		Overview: r.Overview,
		Empty:    r.Overview.Sessions == 0,
	}

	// Top tools — compute percentage so the template doesn't do
	// arithmetic.
	totalToolUses := r.Overview.ToolUses
	for _, t := range r.TopTools {
		pct := 0.0
		if totalToolUses > 0 {
			pct = 100 * float64(t.Count) / float64(totalToolUses)
		}
		page.TopTools = append(page.TopTools, InsightsToolRow{
			ToolName: t.ToolName,
			Count:    t.Count,
			Percent:  pct,
		})
	}

	// Top skills — render last_used as relative time.
	for _, sk := range r.TopSkills {
		page.TopSkills = append(page.TopSkills, InsightsSkillRow{
			Name:     sk.Name,
			Count:    sk.Count,
			LastUsed: relativeTime(sk.LastUsedMs, now),
		})
	}

	// Activity-by-hour — compute bar width as a percent of the
	// busiest hour so the SVG-free CSS bar scales naturally.
	maxCount := 0
	for _, b := range r.ActivityByHour {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}
	for _, b := range r.ActivityByHour {
		width := 0.0
		if maxCount > 0 {
			width = 100 * float64(b.Count) / float64(maxCount)
		}
		page.ActivityByHour = append(page.ActivityByHour, InsightsHourRow{
			Hour:  b.Hour,
			Count: b.Count,
			Width: width,
		})
	}

	// Top sessions — short id + relative timestamp + cwd.
	for _, ts := range r.TopSessions {
		row := InsightsSessionRow{
			SessionID:  ts.SessionID,
			ShortID:    preview.ShortID(ts.SessionID),
			EventCount: ts.EventCount,
			Cwd:        orDashPtr(ts.Cwd),
		}
		row.FirstPrompt = truncatePreviewString(ts.FirstPrompt)
		if ts.StartedAtMs != nil {
			row.Started = time.UnixMilli(*ts.StartedAtMs).UTC().Format("2006-01-02")
		} else {
			row.Started = "-"
		}
		page.TopSessions = append(page.TopSessions, row)
	}

	return page
}
