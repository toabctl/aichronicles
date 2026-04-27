package web

import (
	"net/http"
	"strings"
	"testing"
)

// TestSkillsPage_RendersThreeSections checks the page wires up
// Installed / Invoked / Stale headers + the day-window filter
// chips. We don't seed real SKILL.md files (CollectInstalled
// reads from the user's home dir which the test can't isolate
// safely), so the Installed table just renders its empty-state
// line — what matters is the page returns 200 and shows the
// section structure.
func TestSkillsPage_RendersHeaderAndChips(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/skills")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	for _, want := range []string{
		"Skills",
		`href="/skills?days=14"`,
		`href="/skills?days=30"`,
		"Installed",
		"Invoked recently",
		// no skill_load events seeded → empty-state suggestion
		// for the invoked section.
		"backfill-extractions",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/skills missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestSkillsPage_DaysParamRespected confirms the day-window query
// param flows through to the heading + chip activation.
func TestSkillsPage_DaysParamRespected(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/skills?days=14")
	if !strings.Contains(body, "last 14 days") {
		t.Errorf("days=14 not reflected in heading:\n%s", body)
	}
	if !strings.Contains(body, `href="/skills?days=14" class="agent-chip agent-chip-active"`) {
		t.Errorf("14d chip should be active:\n%s", body)
	}
}
