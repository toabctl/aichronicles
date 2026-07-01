package prompts

import (
	"testing"

	"github.com/toabctl/aichronicles/internal/eval"
)

// These tests are the Layer-1 "fabrication gate": they feed the real
// grounding filters a payload with planted fabrications and assert, via
// internal/eval's fabrication metric, that the shipped grounding drives
// the post-grounding fabrication rate to zero — while confirming it was
// non-zero beforehand (so the assertion can't pass by the filter being a
// no-op). If a future edit weakens a filter, these fail loudly.

func evidenceQuotes(ev []ProposalEvidence) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Quote)
	}
	return out
}

func evidenceSessionIDs(ev []ProposalEvidence) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.SessionID)
	}
	return out
}

func TestGroundingGate_ProposalDropsFabrications(t *testing.T) {
	t.Parallel()
	const (
		realSession  = "11111111-1111-5111-8111-111111111111"
		ghostSession = "99999999-9999-5999-8999-999999999999"
	)
	r := &ProposalResult{Skills: []ProposedSkill{{
		Name: "run-integration-tests",
		// "go test" is a substring of the real evidence quote (grounded);
		// "kubernetes deploy" appears in no quote (fabricated).
		Triggers: []string{"go test", "kubernetes deploy"},
		Evidence: []ProposalEvidence{
			{SessionID: realSession, Quote: "we ran `go test -tags=integration ./...`"},
			{SessionID: ghostSession, Quote: "hallucinated note mentioning kubernetes"},
		},
	}}}
	sk := &r.Skills[0]
	allow := map[string]struct{}{realSession: {}}

	// Pre-grounding: both a fabricated trigger and a fabricated evidence
	// session must be present, or the test proves nothing.
	if eval.Fabrication(sk.Triggers, eval.SubstringGrounder(evidenceQuotes(sk.Evidence))).Clean() {
		t.Fatal("fixture bug: expected a fabricated trigger pre-grounding")
	}
	if eval.Fabrication(evidenceSessionIDs(sk.Evidence), eval.MembershipGrounder([]string{realSession})).Clean() {
		t.Fatal("fixture bug: expected a fabricated evidence session pre-grounding")
	}

	// Apply the shipped grounding in production order (propose_add.go).
	r.GroundTriggers()
	r.GroundEvidence(allow)

	// Post-grounding: every surviving trigger is grounded in a surviving
	// evidence quote, and every surviving evidence session is real.
	if rep := eval.Fabrication(sk.Triggers, eval.SubstringGrounder(evidenceQuotes(sk.Evidence))); !rep.Clean() {
		t.Errorf("post-grounding triggers still fabricated: %v", rep.Ungrounded)
	}
	if rep := eval.Fabrication(evidenceSessionIDs(sk.Evidence), eval.MembershipGrounder([]string{realSession})); !rep.Clean() {
		t.Errorf("post-grounding evidence still fabricated: %v", rep.Ungrounded)
	}

	// And grounding didn't nuke the legitimate atoms.
	if len(sk.Triggers) != 1 || sk.Triggers[0] != "go test" {
		t.Errorf("grounded trigger should survive; got %v", sk.Triggers)
	}
	if len(sk.Evidence) != 1 || sk.Evidence[0].SessionID != realSession {
		t.Errorf("real evidence should survive; got %v", sk.Evidence)
	}
}

func TestGroundingGate_InductionDropsForeignEvidence(t *testing.T) {
	t.Parallel()
	const (
		inducing = "11111111-1111-5111-8111-111111111111"
		foreign  = "22222222-2222-5222-8222-222222222222"
	)
	r := &InductionResult{Skill: &ProposedSkill{
		Name: "deploy-staging",
		Evidence: []ProposalEvidence{
			{SessionID: inducing, Quote: "ran ./deploy.sh staging"},
			{SessionID: foreign, Quote: "evidence from a different session"},
		},
	}}
	member := eval.MembershipGrounder([]string{inducing})

	if eval.Fabrication(evidenceSessionIDs(r.Skill.Evidence), member).Clean() {
		t.Fatal("fixture bug: expected foreign evidence pre-grounding")
	}
	r.GroundInductionEvidence(inducing)
	if rep := eval.Fabrication(evidenceSessionIDs(r.Skill.Evidence), member); !rep.Clean() {
		t.Errorf("post-grounding induction evidence still fabricated: %v", rep.Ungrounded)
	}
	if len(r.Skill.Evidence) != 1 || r.Skill.Evidence[0].SessionID != inducing {
		t.Errorf("inducing-session evidence should survive; got %v", r.Skill.Evidence)
	}
}
