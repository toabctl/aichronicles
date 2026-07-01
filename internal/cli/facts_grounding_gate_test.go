package cli

import (
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/eval"
)

// Layer-1 fabrication gate for induced facts: the shared factGrounded
// predicate (also used by persistInducedFacts) must drop any fact whose
// subject isn't the session cwd or whose quote isn't in the session
// substrate, leaving a post-filter fabrication rate of zero.

func TestFactGrounded(t *testing.T) {
	t.Parallel()
	cwd := "/work/proj"
	substrate := strings.ToLower("we run tests via make test and deploy with deploy.sh")
	cases := []struct {
		name           string
		subject, quote string
		want           bool
	}{
		{"subject matches, quote in substrate", cwd, "make test", true},
		{"subject mismatch", "/evil/proj", "make test", false},
		{"quote not in substrate", cwd, "kubectl apply -f", false},
		{"quote empty is allowed", cwd, "", true},
		{"quote case-insensitive", cwd, "MAKE TEST", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := factGrounded(tc.subject, cwd, tc.quote, substrate); got != tc.want {
				t.Errorf("factGrounded(%q, %q) = %v, want %v", tc.subject, tc.quote, got, tc.want)
			}
		})
	}
}

func TestGroundingGate_FactsDropUnattributable(t *testing.T) {
	t.Parallel()
	cwd := "/work/proj"
	substrate := strings.ToLower("we run tests via make test and deploy with deploy.sh")

	type fact struct{ subject, quote string }
	in := []fact{
		{cwd, "make test"},          // grounded
		{"/evil/proj", "make test"}, // fabricated subject
		{cwd, "kubectl apply -f"},   // quote not in substrate
	}

	// Pre-filter there are fabrications; apply the real predicate.
	var survivors []fact
	for _, f := range in {
		if factGrounded(f.subject, cwd, f.quote, substrate) {
			survivors = append(survivors, f)
		}
	}
	if len(survivors) != 1 || survivors[0] != in[0] {
		t.Fatalf("expected only the grounded fact to survive, got %v", survivors)
	}

	// Metric view of the survivors: subjects all == cwd, quotes all in
	// the substrate → zero fabrication.
	subjects := make([]string, 0, len(survivors))
	quotes := make([]string, 0, len(survivors))
	for _, f := range survivors {
		subjects = append(subjects, f.subject)
		quotes = append(quotes, f.quote)
	}
	if rep := eval.Fabrication(subjects, eval.MembershipGrounder([]string{cwd})); !rep.Clean() {
		t.Errorf("survivor subjects fabricated: %v", rep.Ungrounded)
	}
	if rep := eval.Fabrication(quotes, eval.SubstringGrounder([]string{substrate})); !rep.Clean() {
		t.Errorf("survivor quotes fabricated: %v", rep.Ungrounded)
	}
}
