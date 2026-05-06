package cli

import (
	"testing"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/pkg/api"
)

func TestMergeImpactIntoInvoked_PopulatesMatchingRows(t *testing.T) {
	t.Parallel()
	invoked := []prompts.InvokedSkill{
		{Name: "alpha", Count: 5},
		{Name: "beta", Count: 3},
		{Name: "gamma-no-impact", Count: 1}, // no impact row → unchanged
	}
	impact := []api.SkillImpact{
		{Name: "alpha", TotalLoads: 5, FailedLoads: 1, SuccessRate: 0.8, LastLoadedMs: 1_700_000_000_000},
		{Name: "beta", TotalLoads: 3, FailedLoads: 3, SuccessRate: 0.0, LastLoadedMs: 1_700_000_111_000},
		{Name: "delta-not-invoked", TotalLoads: 9, FailedLoads: 0, SuccessRate: 1.0, LastLoadedMs: 1_700_000_222_000},
	}

	out := mergeImpactIntoInvoked(invoked, impact)

	want := map[string]struct {
		total, fail int
		rate        float64
		lastLoaded  int64
	}{
		"alpha":           {5, 1, 0.8, 1_700_000_000_000},
		"beta":            {3, 3, 0.0, 1_700_000_111_000},
		"gamma-no-impact": {0, 0, 0, 0}, // stays at zero — no impact data
	}
	for _, r := range out {
		w, ok := want[r.Name]
		if !ok {
			t.Errorf("unexpected output row %q", r.Name)
			continue
		}
		if r.TotalLoads != w.total {
			t.Errorf("%q total: got %d want %d", r.Name, r.TotalLoads, w.total)
		}
		if r.FailedLoads != w.fail {
			t.Errorf("%q fail: got %d want %d", r.Name, r.FailedLoads, w.fail)
		}
		if r.SuccessRate != w.rate {
			t.Errorf("%q rate: got %v want %v", r.Name, r.SuccessRate, w.rate)
		}
		if r.LastLoadedMs != w.lastLoaded {
			t.Errorf("%q last_loaded: got %d want %d", r.Name, r.LastLoadedMs, w.lastLoaded)
		}
	}
	if len(out) != len(invoked) {
		t.Errorf("merge should not add rows: got %d, want %d", len(out), len(invoked))
	}
}

func TestMergeImpactIntoInvoked_EmptyImpactIsNoOp(t *testing.T) {
	t.Parallel()
	invoked := []prompts.InvokedSkill{
		{Name: "alpha", Count: 5},
	}
	out := mergeImpactIntoInvoked(invoked, nil)
	if len(out) != 1 || out[0].Name != "alpha" || out[0].TotalLoads != 0 {
		t.Errorf("empty impact should pass through invoked unchanged: %+v", out)
	}
}
