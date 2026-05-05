package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// fakeLLMClientFn wraps a *fakeLLM into the newClient closure
// shape addSkillCandidate expects. Tests that want the fake to
// refuse / approve set toolInput before calling.
func fakeLLMClientFn(f *fakeLLM) func() (llm.Client, error) {
	return func() (llm.Client, error) { return f, nil }
}

// approvedVerification produces a schema-valid go_ahead=true body.
func approvedVerification(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(prompts.ProposalVerification{
		GoAhead: true, Concern: "", Severity: "none", Recommendation: "",
	})
	if err != nil {
		t.Fatalf("marshal approved: %v", err)
	}
	return b
}

// refusedVerification produces a schema-valid go_ahead=false body
// with the given concern + recommendation so tests can assert
// they make it into the user-facing error string.
func refusedVerification(t *testing.T, severity, concern, rec string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(prompts.ProposalVerification{
		GoAhead: false, Concern: concern, Severity: severity, Recommendation: rec,
	})
	if err != nil {
		t.Fatalf("marshal refused: %v", err)
	}
	return b
}

func TestProposeAdd_VerifyApproves_WritesSkill(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())

	dir := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), s, 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	_ = id

	fake := &fakeLLM{toolInput: approvedVerification(t)}
	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"build-test", dir, false, false, fakeLLMClientFn(fake), &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
	}
	if !strings.Contains(out.String(), "verify: ✓ critic approved") {
		t.Errorf("expected approval line in output, got:\n%s", out.String())
	}
	if fake.called != 1 {
		t.Errorf("LLM should be called exactly once on cache miss, got %d", fake.called)
	}
}

func TestProposeAdd_VerifyRefuses_AbortsAndPropagatesConcern(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())

	dir := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), s, 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	_ = id

	fake := &fakeLLM{toolInput: refusedVerification(t,
		"medium",
		"near-duplicate of installed skill 'foo-test'",
		"merge with foo-test instead")}
	var out bytes.Buffer
	err = addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"build-test", dir, false, false, fakeLLMClientFn(fake), &out)
	if err == nil {
		t.Fatal("expected error on critic refusal")
	}
	for _, want := range []string{
		"critic refused",
		"medium",
		"near-duplicate",
		"merge with foo-test",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should carry %q, got: %v", want, err)
		}
	}
	// SKILL.md must NOT exist on disk after a refusal.
	if _, statErr := os.Stat(filepath.Join(dir, "build-test", "SKILL.md")); !os.IsNotExist(statErr) {
		t.Errorf("SKILL.md was written despite critic refusal (stat: %v)", statErr)
	}
}

func TestProposeAdd_VerifyCachesDecision(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	_ = id

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), s, 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}

	fake := &fakeLLM{toolInput: approvedVerification(t)}
	clientFn := fakeLLMClientFn(fake)

	// First apply triggers a fresh verification.
	var out1 bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"build-test", dir1, false, false, clientFn, &out1); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if fake.called != 1 {
		t.Errorf("first apply: LLM should fire once, got %d", fake.called)
	}
	if !strings.Contains(out1.String(), "fresh") {
		t.Errorf("first apply should be marked fresh:\n%s", out1.String())
	}

	// Second apply on the SAME proposal+skill must hit the cache —
	// no second LLM call, output marked "cached".
	var out2 bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"build-test", dir2, false, false, clientFn, &out2); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if fake.called != 1 {
		t.Errorf("second apply: LLM should NOT fire again, got %d total calls", fake.called)
	}
	if !strings.Contains(out2.String(), "cached") {
		t.Errorf("second apply should be marked cached:\n%s", out2.String())
	}
}

func TestProposeAdd_NoVerifyFlag_BypassesCriticEntirely(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	_ = seedProposalOutput(t, s, sampleProposal())

	dir := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), s, 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}

	// Even a fake that would refuse must NEVER be called when
	// noVerify=true is set.
	refusing := &fakeLLM{toolInput: refusedVerification(t, "high", "no", "drop")}
	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"build-test", dir, false, true /* noVerify */, fakeLLMClientFn(refusing), &out); err != nil {
		t.Fatalf("--no-verify path errored: %v", err)
	}
	if refusing.called != 0 {
		t.Errorf("--no-verify must skip the LLM entirely, got %d calls", refusing.called)
	}
	if strings.Contains(out.String(), "critic") {
		t.Errorf("--no-verify output should not mention the critic:\n%s", out.String())
	}
}

func TestProposeVerifyHash_StableAndKeyedOnInputs(t *testing.T) {
	t.Parallel()
	a := proposeVerifyHash(42, "build-test")
	b := proposeVerifyHash(42, "build-test")
	if a != b {
		t.Errorf("same inputs should hash equal: %s vs %s", a, b)
	}
	if c := proposeVerifyHash(43, "build-test"); a == c {
		t.Errorf("different output id must change the hash")
	}
	if c := proposeVerifyHash(42, "ship-test"); a == c {
		t.Errorf("different skill name must change the hash")
	}
}
