package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// TestWriteMergedSkill_RebuildsFrontmatter covers the SKILL.md
// rebuild step in isolation: a MergedSkillResult plus a target
// version yields a SKILL.md whose YAML frontmatter carries the
// AutoSkill metadata and whose body is the LLM-emitted markdown
// verbatim.
//
// Atomicity (tmp-file + rename) is implicit in the helper but
// not asserted here — the test would have to crash mid-write to
// observe partial state, which Go's testing framework can't
// orchestrate cleanly.
func TestWriteMergedSkill_RebuildsFrontmatter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	merged := &prompts.MergedSkillResult{
		Name:         "deploy-staging",
		Description:  "deploy to staging",
		WhenToUse:    "when shipping to staging",
		Triggers:     []string{"deploy staging", "ship staging"},
		Tags:         []string{"deploy", "ci"},
		Examples:     []prompts.ProposedSkillExample{{Input: "ship this branch", Output: "runs deploy"}},
		BodyMarkdown: "# deploy-staging\n\nRun the merged steps.\n",
		Rationale:    "merged candidate's faster path with existing's pre-flight check",
	}

	hash, err := writeMergedSkill(path, merged, "v0.2.0")
	if err != nil {
		t.Fatalf("writeMergedSkill: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	bs := string(body)

	// Frontmatter shape.
	if !strings.HasPrefix(bs, "---\n") {
		t.Errorf("missing opening fence:\n%s", bs)
	}
	for _, want := range []string{
		"name: deploy-staging",
		"description: deploy to staging",
		"when_to_use: when shipping to staging",
		"version: v0.2.0",
		"- deploy",
		"- ci",
		"- deploy staging",
		"- ship staging",
		"input: ship this branch",
		"output: runs deploy",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("expected %q in SKILL.md:\n%s", want, bs)
		}
	}

	// Body markdown lands AFTER the closing fence, not interpolated.
	if !strings.Contains(bs, "# deploy-staging\n\nRun the merged steps.") {
		t.Errorf("body markdown not present verbatim:\n%s", bs)
	}

	// Provenance footer is present and carries the same hash that
	// writeMergedSkill returns. A drift checker can later strip the
	// footer, recompute, and detect post-write tampering.
	if hash == "" {
		t.Errorf("expected non-empty body hash from writeMergedSkill")
	}
	if !strings.Contains(bs, "aichronicles-provenance: sha256:") {
		t.Errorf("missing provenance footer:\n%s", bs)
	}
	if !strings.Contains(bs, hash[:skillProvenanceFingerprintLen]) {
		t.Errorf("footer should embed the leading hex of the returned hash %q:\n%s", hash, bs)
	}

	// File ends with a newline so editors round-trip cleanly.
	if bs[len(bs)-1] != '\n' {
		t.Errorf("expected trailing newline, got %q", bs[len(bs)-1])
	}
}

// TestWriteMergedSkill_EmitsKindInFrontmatter pins the Kind
// propagation fix: a MergedSkillResult carrying the contrastive
// label must land it in the SKILL.md frontmatter so the on-disk
// file stays consistent with skill_candidates.kind. Empty / out-
// of-enum values are omitted (no fabrication).
func TestWriteMergedSkill_EmitsKindInFrontmatter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		kind      string
		wantLine  string // "" → assert NO `kind:` appears
		forbidden string
	}{
		{name: "pattern emitted", kind: "pattern", wantLine: "kind: pattern"},
		{name: "pitfall emitted", kind: "pitfall", wantLine: "kind: pitfall"},
		{name: "empty omitted", kind: "", forbidden: "kind:"},
		{name: "out-of-enum omitted", kind: "garbage", forbidden: "kind:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "SKILL.md")
			merged := &prompts.MergedSkillResult{
				Name:         "x",
				Description:  "d",
				WhenToUse:    "w",
				Kind:         tc.kind,
				BodyMarkdown: "# x\n",
			}
			if _, err := writeMergedSkill(path, merged, "v0.1.0"); err != nil {
				t.Fatalf("writeMergedSkill: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			bs := string(body)
			if tc.wantLine != "" && !strings.Contains(bs, tc.wantLine) {
				t.Errorf("expected %q in:\n%s", tc.wantLine, bs)
			}
			if tc.forbidden != "" && strings.Contains(bs, tc.forbidden) {
				t.Errorf("did not expect %q in:\n%s", tc.forbidden, bs)
			}
		})
	}
}

// TestWriteMergedSkill_HashCoversBodyOnly pins the SSGM relationship
// the drift check depends on: the returned hash is sha256(body
// without the trailing provenance footer), so a verifier that
// strips the footer and recomputes lands on the same value.
// Without this contract, every merged file would look "drifted" the
// moment it was written.
func TestWriteMergedSkill_HashCoversBodyOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	merged := &prompts.MergedSkillResult{
		Name:         "x",
		Description:  "x",
		WhenToUse:    "now",
		BodyMarkdown: "# x\n\nbody\n",
	}
	hash, err := writeMergedSkill(path, merged, "v0.1.0")
	if err != nil {
		t.Fatalf("writeMergedSkill: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stripped := stripProvenanceFooter(string(raw))
	got := sha256.Sum256([]byte(stripped))
	if hex.EncodeToString(got[:]) != hash {
		t.Errorf("hash drift:\n  on-disk(stripped) sha256 = %x\n  writeMergedSkill returned = %s", got, hash)
	}
}

// stripProvenanceFooter removes the trailing aichronicles-provenance
// HTML comment and the EXTRA leading newline the footer prepends,
// mirroring what a drift check would do before recomputing the
// body hash. The body's own trailing newline (always present —
// writeMergedSkill ensures it) is preserved.
//
// Layout written to disk: body + "\n<!-- ... -->\n". Body already
// ends with "\n", so the marker's leading "\n" is bonus whitespace
// that was NOT part of the hashed input.
func stripProvenanceFooter(s string) string {
	const marker = "\n<!-- aichronicles-provenance:"
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return s
	}
	return s[:i] // drop the marker's leading \n; body's own \n is at i-1
}

// TestRenderMergedScriptScaffold pins the merge-side script
// scaffolder. Same shape as the add-side scaffolder but the
// header notes "merged" provenance — a `grep "Scaffolded by"`
// over the user's skills directory tells which path produced each
// file. Three branches: steps[], free-form body, and TODO fallback.
func TestRenderMergedScriptScaffold(t *testing.T) {
	t.Parallel()

	t.Run("steps with placeholders", func(t *testing.T) {
		sc := &prompts.ProposedSkillScript{
			Name:    "preflight.sh",
			Purpose: "verify cluster",
			Steps: []prompts.ProposedScriptStep{
				{Cmd: "kubectl --context={cluster} get nodes", Purpose: "cluster reachable"},
			},
			Placeholders: []prompts.ProposedScriptPlaceholder{
				{Token: "cluster", Description: "k8s context", Example: "staging-eu1"},
			},
		}
		body := renderMergedScriptScaffold(sc, "deploy-staging", 99)
		for _, want := range []string{
			"#!/usr/bin/env bash",
			"# verify cluster",
			"# Skill: deploy-staging",
			"propose merge",
			"id=99",
			"set -euo pipefail",
			"kubectl --context={cluster} get nodes",
			"# cluster reachable",
			"{cluster}",
			"staging-eu1",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("expected %q in scaffold:\n%s", want, body)
			}
		}
	})

	t.Run("free-form body", func(t *testing.T) {
		sc := &prompts.ProposedSkillScript{
			Name: "x.sh", Purpose: "do x", Body: "echo hello",
		}
		body := renderMergedScriptScaffold(sc, "x", 1)
		if !strings.Contains(body, "echo hello") {
			t.Errorf("body not echoed:\n%s", body)
		}
	})

	t.Run("TODO fallback", func(t *testing.T) {
		sc := &prompts.ProposedSkillScript{Name: "x.sh", Purpose: "do x"}
		body := renderMergedScriptScaffold(sc, "x", 1)
		if !strings.Contains(body, "TODO: implement") {
			t.Errorf("expected TODO fallback:\n%s", body)
		}
	})
}

// TestMergeProposedSkill_RefusesSelfMerge pins the safety check the
// merge path runs BEFORE the LLM round-trip: if LoadAddedSkillCandidate
// returns the same row we're about to mark decision='merge' (same
// llm_output_id + skill_name), bail with a clear error rather than
// produce a row whose merged_into_id points at itself. That state is
// reachable because LoadAddedSkillCandidate filters by skill_name
// only, and the schema's REFERENCES skill_candidates(id) doesn't
// reject self-FKs.
func TestMergeProposedSkill_RefusesSelfMerge(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, err := loadLatestProposal(t.Context(), s, id)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}

	// Plant the candidate row in `decision='add'` state, then plant
	// a SKILL.md on disk. This is exactly the world the user is in
	// when they ran `propose add` and now mistakenly try
	// `propose merge` against the same output.
	if err := store.RecordSkillCandidate(t.Context(), s.DB(), id, "build-test", 1_700_000_000_000); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	root := t.TempDir()
	skillDir := filepath.Join(root, "build-test")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	skillMd := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillMd, []byte("---\nname: build-test\nversion: v0.1.0\n---\nbody\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := store.MarkSkillCandidateAdded(t.Context(), s.DB(), id, "build-test", skillMd, 1_700_000_500_000); err != nil {
		t.Fatalf("mark add: %v", err)
	}

	var out bytes.Buffer
	err = mergeProposedSkill(
		t.Context(), s, apiForStore(t, s), result, id, 0, "build-test", root,
		true, // noVerify — the refusal must fire before any LLM work
		nilLLMClient, &out,
	)
	if err == nil {
		t.Fatalf("expected self-merge refusal, got nil error (output: %q)", out.String())
	}
	if !strings.Contains(err.Error(), "already added") {
		t.Errorf("expected error to mention 'already added', got: %v", err)
	}
	if !strings.Contains(err.Error(), "build-test") {
		t.Errorf("expected error to mention skill name, got: %v", err)
	}

	// And the candidate row must still be in its prior state — the
	// refusal happens before any UPDATE.
	rows, err := store.LoadSkillCandidatesByName(t.Context(), s.DB(), "build-test", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	if rows[0].Decision != store.MaintenanceAdd {
		t.Errorf("decision drifted: got %q, want %q", rows[0].Decision, store.MaintenanceAdd)
	}
	if rows[0].MergedIntoID.Valid {
		t.Errorf("merged_into_id should be NULL after refusal, got %d", rows[0].MergedIntoID.Int64)
	}
}

// TestProposeMergeHash_StableOnIdenticalInputs pins the merge
// cache key invariant: identical (outputID, name, currentSkillMd,
// nextVersion) produce identical hashes, while any single
// component drift produces a different hash. Together with
// LLMKindSkillMerge keying, this is what makes a re-run on the
// same proposal free.
func TestProposeMergeHash_StableOnIdenticalInputs(t *testing.T) {
	t.Parallel()
	a := proposeMergeHash(42, "deploy-staging", "---\nname: x\n---\nbody", "v0.1.1")
	b := proposeMergeHash(42, "deploy-staging", "---\nname: x\n---\nbody", "v0.1.1")
	if a != b {
		t.Errorf("expected stable hash, got %s vs %s", a, b)
	}
	c := proposeMergeHash(43, "deploy-staging", "---\nname: x\n---\nbody", "v0.1.1")
	if a == c {
		t.Errorf("hash collided across different output IDs")
	}
	d := proposeMergeHash(42, "deploy-staging", "---\nname: x\n---\ndifferent body", "v0.1.1")
	if a == d {
		t.Errorf("hash collided across different SKILL.md bodies (expected hand-edit invalidation)")
	}
	e := proposeMergeHash(42, "deploy-staging", "---\nname: x\n---\nbody", "v0.1.2")
	if a == e {
		t.Errorf("hash collided across different next_version values")
	}
}
