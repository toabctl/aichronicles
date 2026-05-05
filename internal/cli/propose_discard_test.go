package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
)

// TestProposeDiscard_RecordsLifecycle covers the happy path: an
// extracted candidate gets its decision flipped to MaintenanceDiscard
// when the user explicitly discards it, with the decision_at_ms set.
// Future propose runs see the rejection signal.
func TestProposeDiscard_RecordsLifecycle(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(t.Context(), apiForStore(t, s), id)

	// Pre-record the candidate row (canonical post-extraction state).
	if err := store.RecordSkillCandidate(t.Context(), s.DB(), id, "build-test", 1_700_000_000_000); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	var out bytes.Buffer
	if err := discardProposedSkill(t.Context(), apiForStore(t, s), result, id, 1_700_000_000_000, "build-test", &out); err != nil {
		t.Fatalf("discardProposedSkill: %v", err)
	}

	rows, err := store.LoadSkillCandidatesByName(t.Context(), s.DB(), "build-test", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	r := rows[0]
	if r.Decision != store.MaintenanceDiscard {
		t.Errorf("decision: got %q want %q", r.Decision, store.MaintenanceDiscard)
	}
	if !r.DecisionAtMs.Valid {
		t.Errorf("decision_at_ms not set after discard")
	}
	if r.AddPath.Valid {
		t.Errorf("add_path should be empty on discard, got %q", r.AddPath.String)
	}
	if !strings.Contains(out.String(), "discarded build-test") {
		t.Errorf("output missing discarded message: %s", out.String())
	}
}

// TestProposeDiscard_AutoInsertsMissingCandidate covers the
// fallback: a discard against a candidate row that doesn't exist
// yet (the candidate predates the lifecycle index, or was created
// via a path that doesn't seed it) auto-inserts the row before
// flipping the decision.
func TestProposeDiscard_AutoInsertsMissingCandidate(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(t.Context(), apiForStore(t, s), id)

	// No RecordSkillCandidate call beforehand.
	var out bytes.Buffer
	if err := discardProposedSkill(t.Context(), apiForStore(t, s), result, id, 1_700_000_000_000, "build-test", &out); err != nil {
		t.Fatalf("discardProposedSkill: %v", err)
	}

	rows, err := store.LoadSkillCandidatesByName(t.Context(), s.DB(), "build-test", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1 (auto-insert path failed)", len(rows))
	}
	if rows[0].Decision != store.MaintenanceDiscard {
		t.Errorf("decision: got %q want %q", rows[0].Decision, store.MaintenanceDiscard)
	}
}

// TestProposeDiscard_RejectsUnknownSkill asserts a discard against
// a name not in the proposal fails fast — preventing the user from
// recording a discard for the wrong skill via a typo.
func TestProposeDiscard_RejectsUnknownSkill(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(t.Context(), apiForStore(t, s), id)

	var out bytes.Buffer
	if err := discardProposedSkill(t.Context(), apiForStore(t, s), result, id, 0, "no-such-skill", &out); err == nil {
		t.Errorf("expected error for unknown skill")
	}
}
