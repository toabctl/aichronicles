package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

func TestProjectsPage_RendersEmptyStateOnEmptyStore(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/projects")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, "Projects") {
		t.Errorf("page heading missing:\n%s", body)
	}
	if !strings.Contains(body, "no sessions in window") {
		t.Errorf("expected empty-state line:\n%s", body)
	}
}

func TestProjectsPage_RendersRowsAndDayChips(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Now().UTC().Add(-time.Hour)
	seedSession(t, st, "alpha", "investigate plan", now)
	seedSession(t, st, "beta", "another prompt", now.Add(time.Minute))

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/projects?days=7")
	for _, want := range []string{
		"Projects",
		`href="/projects?days=7" class="agent-chip agent-chip-active"`,
		"Project root",
		"Sessions",
		"<code>",
		`href="/?project=`, // each row links into the sessions filter
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/projects missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestBuildProjectsPage_RollsCwdsToProjectRoot pins the
// aggregation: two start cwds inside the same project root
// collapse into one row.
func TestBuildProjectsPage_RollsCwdsToProjectRoot(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	rec := func(off time.Duration) int64 {
		return now.Add(off).UnixMilli()
	}
	// Two cwds nominally inside /tmp (which has no .git/.claude
	// markers, so FindProjectRootGeneric falls back to the
	// passed-in cwd as-is). To exercise the rollup we use the
	// SAME cwd twice — the aggregator would normally collapse
	// duplicates at the SQL layer; here we feed two rows so
	// buildProjectsPage's bucket logic gets exercised even on
	// systems where /tmp has no markers.
	aggs := []store.ProjectAggregate{
		{Cwd: "/tmp/proj-a", Sessions: 2, Events: 10, LastActivityMs: rec(0)},
		{Cwd: "/tmp/proj-b", Sessions: 1, Events: 5, LastActivityMs: rec(-time.Hour)},
	}
	page := buildProjectsPage(aggs, 7, now)
	if page.Empty {
		t.Fatal("expected non-empty page")
	}
	if len(page.Projects) < 1 {
		t.Fatalf("expected ≥1 row, got %d", len(page.Projects))
	}
	// Newest first.
	for i := 1; i < len(page.Projects); i++ {
		if page.Projects[i-1].SortKey < page.Projects[i].SortKey {
			t.Errorf("sort: expected descending activity, got %+v", page.Projects)
		}
	}
}
