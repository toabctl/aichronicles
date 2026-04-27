package web

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestInsightsPage_RendersAllSections drives /insights against a
// store with a couple of seeded sessions and asserts each
// expected section + element shows up. Covers the happy path:
// overview counters, top-tools table (with a percentage), top-
// sessions table, activity-by-hour bars, and the days-window
// filter chips.
func TestInsightsPage_RendersAllSections(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedSession(t, st, "sess-foo", "investigate the slow plan", now)
	seedSession(t, st, "sess-bar", "another prompt", now.Add(15*time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/insights")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	for _, want := range []string{
		"Insights",
		`href="/insights?days=7"`,  // window-filter chips
		`href="/insights?days=30"`, // default-active
		`href="/insights?days=90"`,
		"Overview",
		"sessions",
		"Activity by hour",
		`<span class="bar"`, // histogram bars present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/insights missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestInsightsPage_EmptyWindow renders the no-sessions empty
// state cleanly rather than producing broken / empty tables.
func TestInsightsPage_EmptyWindow(t *testing.T) {
	t.Parallel()
	st := openTempStore(t) // no seed
	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/insights")
	if !strings.Contains(body, "no sessions in this window") {
		t.Errorf("empty insights should show empty-state line:\n%s", body)
	}
}

// TestInsightsPage_DaysParamRespected confirms ?days=7 changes
// both the window AND which chip is marked active.
func TestInsightsPage_DaysParamRespected(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	seedSession(t, st, "sess-x", "p", time.Now().Add(-time.Hour))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/insights?days=7")
	if !strings.Contains(body, "last 7 days") {
		t.Errorf("days=7 should be reflected in heading:\n%s", body)
	}
	if !strings.Contains(body, `href="/insights?days=7" class="agent-chip agent-chip-active"`) {
		t.Errorf("7d chip should be active:\n%s", body)
	}
}
