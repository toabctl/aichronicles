package cli

import (
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// TestSkillCandidateDecisionsAgreeOnWire asserts that wire.Decision*
// and store.Maintenance* serialise to the same literal strings. The
// two enums live in packages that cannot import each other (wire is
// HTTP-contract-only; store is SQL-only), so the only way a rename
// on one side could silently drift from the other is for this test
// to be missing.
func TestSkillCandidateDecisionsAgreeOnWire(t *testing.T) {
	t.Parallel()
	pairs := []struct {
		name  string
		wire  wire.SkillCandidateDecision
		store store.MaintenanceAction
	}{
		{"pending", wire.DecisionPending, store.MaintenancePending},
		{"add", wire.DecisionAdd, store.MaintenanceAdd},
		{"merge", wire.DecisionMerge, store.MaintenanceMerge},
		{"discard", wire.DecisionDiscard, store.MaintenanceDiscard},
	}
	for _, p := range pairs {
		if string(p.wire) != string(p.store) {
			t.Errorf("%s: wire=%q store=%q — strings must agree across the wire",
				p.name, string(p.wire), string(p.store))
		}
	}
}
