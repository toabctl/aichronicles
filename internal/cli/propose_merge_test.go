package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	if err := writeMergedSkill(path, merged, "v0.2.0"); err != nil {
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

	// File ends with a newline so editors round-trip cleanly.
	if bs[len(bs)-1] != '\n' {
		t.Errorf("expected trailing newline, got %q", bs[len(bs)-1])
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
