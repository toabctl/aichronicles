package web

import (
	"net/http"
	"strings"
	"testing"
)

// TestSkillsPage_RendersHeaderAndChips checks the page wires up
// the day-window filter chips and the Invoked / Stale section
// structure. We don't assert on the "Installed" header word
// because the template hides it when there are no SKILL.md files
// on disk, and the test runner has no control over $HOME (CI
// runners have no skills installed; developer machines do — the
// assertion would be flaky either way). The "Invoked" header IS
// always rendered, with an empty-state line when no skill_load
// extractions exist in the window — that's the structural
// evidence we lean on here.
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
