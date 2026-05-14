package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
)

// insightsDefaultDays matches `aichronicles insights`'s default
// window. Configurable per-request via ?days=<n>.
const insightsDefaultDays = 30

// insightsHandler renders /insights — the same overview /
// top-tools / top-skills / activity-by-hour / top-sessions data
// the CLI prints, as HTML. Pure SQL aggregation; no LLM call.
func (s *Server) insightsHandler(w http.ResponseWriter, r *http.Request) {
	rawDays, _ := strconv.Atoi(r.URL.Query().Get("days"))
	sinceMs, days := timefmt.SinceMsFromDays(rawDays, insightsDefaultDays, 365, time.Now())

	report, err := s.api.Insights(r.Context(), apiclient.InsightsRequest{SinceMs: sinceMs})
	if err != nil {
		s.internalError(w, "insightsHandler: load", "could not load insights", err)
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

	// Activity-by-hour — bucket bar width into 0–10 (deciles of the
	// busiest hour's count) so the template can pick a CSS class
	// rather than emit an inline `style="width:…"` attribute that
	// the strict CSP would block.
	maxCount := 0
	for _, b := range r.ActivityByHour {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}
	for _, b := range r.ActivityByHour {
		page.ActivityByHour = append(page.ActivityByHour, InsightsHourRow{
			Hour:        b.Hour,
			Count:       b.Count,
			WidthBucket: hourWidthBucket(b.Count, maxCount),
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

// hourWidthBucket maps an hour's count onto 0..10 deciles of
// maxCount. Pairs with .bar-w-0…bar-w-10 classes in app.css.
// A non-zero count always returns at least 1 so the bar
// renders a visible sliver instead of disappearing.
func hourWidthBucket(count, maxCount int) int {
	if maxCount <= 0 || count <= 0 {
		return 0
	}
	bucket := count * 10 / maxCount
	if bucket < 1 {
		bucket = 1
	}
	if bucket > 10 {
		bucket = 10
	}
	return bucket
}
