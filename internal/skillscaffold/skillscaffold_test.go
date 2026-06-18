package skillscaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
)

// sampleSkill is the build-test fixture the rendering tests share:
// a skill with one helper script carrying a Body, plus two evidence
// rows. Mirrors the cli package's sampleProposal()[0] so the format
// assertions stay anchored to a realistic proposal.
func sampleSkill() *prompts.ProposedSkill {
	return &prompts.ProposedSkill{
		Name:                 "build-test",
		WhenToUse:            "Use when building and testing a Go service from scratch.",
		Why:                  "We've done this 4 times in the last week with the same steps.",
		AlternativesRejected: "considered as a CLAUDE.md rule but workflow has multiple steps",
		Evidence: []prompts.ProposalEvidence{
			{SessionID: "abc12345-aaaa-bbbb-cccc-dddddddddddd", Quote: "q1", WhatHappened: "user investigated thing X"},
			{SessionID: "def67890-aaaa-bbbb-cccc-dddddddddddd", Quote: "q2", WhatHappened: "user investigated thing X again"},
		},
		Frequency: 2,
		Effort:    "medium",
		Scripts: []prompts.ProposedSkillScript{
			{
				Name:    "run-checks.sh",
				Purpose: "Run gofmt + vet + tests + race in one shot.",
				Body:    "go fmt ./...\ngo vet ./...\ngo test -race ./...",
			},
		},
	}
}

// TestRender_FrontmatterMatchesClaudeCodeDocs pins the canonical
// SKILL.md format documented at https://code.claude.com/docs/en/skills:
//   - name is kebab-case (NOT title-case);
//   - description and when_to_use live in separate frontmatter fields;
//   - the body has a single H1 + a "## Steps" section, and does NOT
//     carry invented "When to apply" / "Why" / "Pitfalls" /
//     "Verification" headers.
func TestRender_FrontmatterMatchesClaudeCodeDocs(t *testing.T) {
	t.Parallel()
	body := Render(sampleSkill(), 7).Body

	for _, want := range []string{
		"---\n",
		"name: build-test\n", // kebab-case, NOT "Build Test"
		"description:",
		"when_to_use:",
		"\n# build-test\n", // H1 matches the kebab name
		"## Steps\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scaffold missing %q\n--- body ---\n%s", want, body)
		}
	}

	for _, unwanted := range []string{
		"## When to apply",
		"## Why",
		"## Pitfalls",
		"## Verification",
		"## Helper scripts",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("canonical scaffold should NOT contain %q\n--- body ---\n%s",
				unwanted, body)
		}
	}
}

// TestRender_ScriptReferencedInline confirms that when a skill has
// a script, it gets cited inline in Steps as `Run scripts/<name> to
// <purpose>` — matching the docs' "Reference supporting files from
// your SKILL.md" convention.
func TestRender_ScriptReferencedInline(t *testing.T) {
	t.Parallel()
	body := Render(sampleSkill(), 7).Body
	want := "Run `scripts/run-checks.sh` to run gofmt + vet + tests + race in one shot"
	if !strings.Contains(body, want) {
		t.Errorf("scaffold should reference the script inline:\nwant substring: %s\n--- body ---\n%s",
			want, body)
	}
}

// TestRender_FrontmatterParsesAsValidYAML confirms the
// yaml.v3-marshalled frontmatter survives a round-trip and carries
// the expected fields, even with quoting-hostile input.
func TestRender_FrontmatterParsesAsValidYAML(t *testing.T) {
	t.Parallel()
	sk := prompts.ProposedSkill{
		Name:                 "tricky-skill",
		WhenToUse:            `When the user types "build" or 'test' or has: a colon, # hash, or backtick.`,
		Why:                  "Multi-line\nbecomes\nfine through yaml.v3.",
		AlternativesRejected: "n/a",
		Evidence: []prompts.ProposalEvidence{
			{SessionID: "s1", Quote: "q", WhatHappened: "h"},
			{SessionID: "s2", Quote: "q", WhatHappened: "h"},
		},
		Frequency: 2, Effort: "small",
	}
	body := Render(&sk, 1).Body

	fm, _, ok := strings.Cut(strings.TrimPrefix(body, "---\n"), "---\n")
	if !ok {
		t.Fatalf("could not find closing --- in:\n%s", body)
	}

	var parsed Frontmatter
	if err := yaml.Unmarshal([]byte(fm), &parsed); err != nil {
		t.Fatalf("frontmatter not valid YAML: %v\n%s", err, fm)
	}
	if parsed.Name != "tricky-skill" {
		t.Errorf("name: got %q, want tricky-skill", parsed.Name)
	}
	if !strings.Contains(parsed.Description, "Multi-line") {
		t.Errorf("description should round-trip: %q", parsed.Description)
	}
	if !strings.Contains(parsed.WhenToUse, "build") {
		t.Errorf("when_to_use should round-trip: %q", parsed.WhenToUse)
	}
}

// TestRender_FullEqualsBodyPlusFooterAndHashIsReversible pins the
// add-path invariant: Full is Body + ProvenanceFooter(SHA256), and
// SHA256 is the hash of Body alone so a drift checker can strip the
// footer, recompute, and match.
func TestRender_FullEqualsBodyPlusFooterAndHashIsReversible(t *testing.T) {
	t.Parallel()
	r := Render(sampleSkill(), 42)

	sum := sha256.Sum256([]byte(r.Body))
	if got := hex.EncodeToString(sum[:]); got != r.SHA256 {
		t.Errorf("SHA256 should hash the pre-footer body: got %q want %q", r.SHA256, got)
	}
	if r.Full != r.Body+ProvenanceFooter(r.SHA256) {
		t.Errorf("Full should equal Body + ProvenanceFooter(SHA256)")
	}
	if recovered := strings.TrimSuffix(r.Full, ProvenanceFooter(r.SHA256)); recovered != r.Body {
		t.Errorf("stripping the footer should recover the hashed body")
	}
}

// TestProvenanceFooter_Format pins the marker the drift checker
// keys off, plus the fingerprint length budget.
func TestProvenanceFooter_Format(t *testing.T) {
	t.Parallel()
	footer := ProvenanceFooter("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	for _, want := range []string{
		"aichronicles-provenance:",
		"sha256:0123456789ab",          // exactly 12 chars of fingerprint
		"`aichronicles skills verify`", // hint at the future drift-check command
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q\n%s", want, footer)
		}
	}
	// Short hash inputs (e.g. an empty hash) shouldn't panic.
	if got := ProvenanceFooter(""); !strings.Contains(got, "sha256:") {
		t.Errorf("empty-hash footer should still emit the marker, got: %s", got)
	}
}

// TestRenderScript_EmptyBodyTODO confirms the LLM can omit `body`
// and we fall back to a TODO stub rather than inventing an empty
// script.
func TestRenderScript_EmptyBodyTODO(t *testing.T) {
	t.Parallel()
	sk := &prompts.ProposedSkill{Name: "x"}
	sc := &prompts.ProposedSkillScript{Name: "stub.sh", Purpose: "stub purpose"}
	body := RenderScript(sc, sk, 99)
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"# stub purpose",
		"# Skill: x",
		"TODO: implement",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// TestFrontmatterKind_NormalisesOutOfEnum mirrors the store-side
// normalisation: recognised values pass through, everything else
// (including a hallucinated label) collapses to empty so the
// frontmatter never carries a guessed kind.
func TestFrontmatterKind_NormalisesOutOfEnum(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pattern": "pattern",
		"pitfall": "pitfall",
		"":        "",
		"PATTERN": "",
		"insight": "",
	}
	for in, want := range cases {
		if got := FrontmatterKind(in); got != want {
			t.Errorf("FrontmatterKind(%q) = %q, want %q", in, got, want)
		}
	}
}
