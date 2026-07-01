package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
)

// Layer-4 regression: snapshot the POST-GROUNDING structured output of
// each pipeline given a fixed LLM tool-input body. These lock in what
// actually gets persisted after parse + grounding, so an edit to a
// prompt, schema, or grounding filter that changes the surviving atoms
// is caught as a golden diff (regenerate deliberately with
// -update-golden). The fixtures deliberately include ungrounded atoms;
// the golden files must show them removed.

func mustMarshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestGolden_ProposalPostGrounding(t *testing.T) {
	t.Parallel()
	const real = "11111111-1111-5111-8111-111111111111"
	// One grounded trigger ("go test") + one fabricated ("kubernetes
	// deploy"); one real evidence session + one ghost. Post-grounding the
	// fabricated trigger and the ghost evidence must be gone.
	body := `{
	  "skills": [{
	    "name": "run-integration-tests",
	    "when_to_use": "before pushing changes that touch the store",
	    "why": "integration tests catch UDS/SQLite seams unit tests miss",
	    "triggers": ["go test", "kubernetes deploy"],
	    "evidence": [
	      {"session_id": "` + real + `", "quote": "we ran ` + "`go test -tags=integration ./...`" + `", "what_happened": "ran the integration suite"},
	      {"session_id": "99999999-9999-5999-8999-999999999999", "quote": "hallucinated note mentioning kubernetes", "what_happened": "n/a"}
	    ],
	    "frequency": 3,
	    "effort": "low",
	    "alternatives_rejected": "running the full suite every time"
	  }]
	}`
	var result prompts.ProposalResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	// Production order (propose_add.go): triggers then evidence.
	result.GroundTriggers()
	result.GroundEvidence(map[string]struct{}{real: {}})

	assertGolden(t, "proposal_post_grounding.json", mustMarshalIndent(t, result))
}

func TestGolden_InductionPostGrounding(t *testing.T) {
	t.Parallel()
	const inducing = "11111111-1111-5111-8111-111111111111"
	body := `{
	  "skill": {
	    "name": "deploy-staging",
	    "when_to_use": "deploying a service to staging",
	    "why": "the deploy script encodes the right flags",
	    "triggers": ["deploy.sh"],
	    "evidence": [
	      {"session_id": "` + inducing + `", "quote": "ran ./deploy.sh staging", "what_happened": "deployed to staging"},
	      {"session_id": "22222222-2222-5222-8222-222222222222", "quote": "evidence from a different session", "what_happened": "n/a"}
	    ],
	    "frequency": 1,
	    "effort": "low",
	    "alternatives_rejected": "manual kubectl"
	  },
	  "rationale": "extracted a deploy-staging skill from the concrete commands"
	}`
	var result prompts.InductionResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	result.GroundInductionEvidence(inducing)
	// The grounded trigger must survive too (deploy.sh ∈ the inducing quote).
	result.Skill.Triggers = prompts.FilterGroundedTriggers(result.Skill.Triggers, result.Skill.Evidence)

	assertGolden(t, "induction_post_grounding.json", mustMarshalIndent(t, result))
}

func TestGolden_FactsPostGrounding(t *testing.T) {
	t.Parallel()
	const cwd = "/work/proj"
	substrate := strings.ToLower("we run tests via make test and deploy with deploy.sh")
	body := `{
	  "found": true,
	  "facts": [
	    {"subject": "` + cwd + `", "predicate": "runs_tests_via", "object": "make test", "confidence": 0.9, "quote": "make test", "what_happened": "ran the suite"},
	    {"subject": "/evil/proj", "predicate": "runs_tests_via", "object": "make test", "confidence": 0.9, "quote": "make test", "what_happened": "foreign subject"},
	    {"subject": "` + cwd + `", "predicate": "deploys_via", "object": "kubectl", "confidence": 0.9, "quote": "kubectl apply -f", "what_happened": "quote not in substrate"}
	  ],
	  "rationale": "extracted build/deploy facts"
	}`
	var result prompts.FactsResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	// Apply the shared production drop (persistInducedFacts uses the same
	// predicate) and snapshot the survivors.
	kept := result.Facts[:0]
	for _, f := range result.Facts {
		if factGrounded(f.Subject, cwd, f.Quote, substrate) {
			kept = append(kept, f)
		}
	}
	result.Facts = kept

	assertGolden(t, "facts_post_grounding.json", mustMarshalIndent(t, result))
}
