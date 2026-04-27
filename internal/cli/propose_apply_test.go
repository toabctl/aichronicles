package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// seedProposalOutput inserts one llm_outputs row with kind=propose
// carrying the given ProposalResult body. Returns the row id.
// Mirrors how runCachedLLM persists a fresh propose call.
func seedProposalOutput(t *testing.T, s *store.Store, result *prompts.ProposalResult) int64 {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:        store.LLMKindPropose,
		Model:       "test-model",
		PromptHash:  "h-test-" + t.Name(),
		Body:        string(body),
		CreatedAtMs: time.Now().UnixMilli(),
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("save: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// sampleProposal builds a ProposalResult covering both shapes the
// apply path needs to handle: a plain skill (no scripts) and a
// skill that carries a helper script.
func sampleProposal() *prompts.ProposalResult {
	ev := []prompts.ProposalEvidence{
		{SessionID: "abc12345-aaaa-bbbb-cccc-dddddddddddd",
			Quote:        "first session quote",
			WhatHappened: "user investigated thing X"},
		{SessionID: "def67890-aaaa-bbbb-cccc-dddddddddddd",
			Quote:        "second session quote",
			WhatHappened: "user investigated thing X again"},
	}
	return &prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{
			{
				Name:                 "build-test",
				WhenToUse:            "Use when building and testing a Go service from scratch.",
				Why:                  "We've done this 4 times in the last week with the same steps.",
				Evidence:             ev,
				Frequency:            2,
				Effort:               "medium",
				AlternativesRejected: "considered as a CLAUDE.md rule but workflow has multiple steps",
				Scripts: []prompts.ProposedSkillScript{
					{
						Name:    "run-checks.sh",
						Purpose: "Run gofmt + vet + tests + race in one shot.",
						Body:    "go fmt ./...\ngo vet ./...\ngo test -race ./...",
					},
				},
			},
			{
				Name:                 "another-skill",
				WhenToUse:            "When doing another thing.",
				Evidence:             ev,
				Frequency:            2,
				Effort:               "small",
				AlternativesRejected: "n/a",
			},
		},
	}
}

func TestProposeApply_SkillWritesScaffoldAndScripts(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())

	dir := t.TempDir()
	result, output, err := loadLatestProposal(context.Background(), s, 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	if output.ID != id {
		t.Errorf("loaded id %d, want %d", output.ID, id)
	}

	var out bytes.Buffer
	if err := applyProposedSkill(result, output.ID, "build-test", dir, false, &out); err != nil {
		t.Fatalf("applyProposedSkill: %v", err)
	}

	skillMd := filepath.Join(dir, "build-test", "SKILL.md")
	body, err := os.ReadFile(skillMd)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	for _, want := range []string{
		"---",
		"name: build-test", // kebab-case per Claude Code docs
		"description:",
		"when_to_use:",
		"# build-test",
		"## Steps",
		"TODO",
		"`scripts/run-checks.sh`",
		"abc12345-aaaa",
		"def67890-aaaa",
		"considered as a CLAUDE.md rule but workflow has multiple steps",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("SKILL.md missing %q\n--- body ---\n%s", want, string(body))
		}
	}

	scriptPath := filepath.Join(dir, "build-test", "scripts", "run-checks.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat script: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("script not executable, mode=%o", info.Mode().Perm())
	}
	scriptBody, _ := os.ReadFile(scriptPath)
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"# Run gofmt + vet + tests + race in one shot.",
		"# Skill: build-test",
		"set -euo pipefail",
		"go test -race ./...",
	} {
		if !strings.Contains(string(scriptBody), want) {
			t.Errorf("script missing %q\n--- body ---\n%s", want, string(scriptBody))
		}
	}

	stdout := out.String()
	for _, want := range []string{
		"wrote ",
		"executable",
		"abc12345",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q: %s", want, stdout)
		}
	}
}

// TestProposeApply_SkillWithoutScripts confirms the no-scripts
// branch still works (no scripts/ dir, no Helper-scripts section).
func TestProposeApply_SkillWithoutScripts(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), s, id)

	dir := t.TempDir()
	var out bytes.Buffer
	if err := applyProposedSkill(result, id, "another-skill", dir, false, &out); err != nil {
		t.Fatalf("applyProposedSkill: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "another-skill", "SKILL.md"))
	if strings.Contains(string(body), "Run `scripts/") {
		t.Errorf("no-scripts skill should not reference scripts inline:\n%s", string(body))
	}
	if _, err := os.Stat(filepath.Join(dir, "another-skill", "scripts")); !os.IsNotExist(err) {
		t.Errorf("no-scripts skill should not create scripts/ dir, err=%v", err)
	}
}

// TestProposeApply_RefusesOverwriteWithoutForce: writing twice
// must fail unless --force is passed. Pin the invariant directly.
// Also covers the case where the SKILL.md is fine but a script
// already exists — must refuse before writing anything.
func TestProposeApply_RefusesOverwriteWithoutForce(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), s, id)

	dir := t.TempDir()
	var out bytes.Buffer

	if err := applyProposedSkill(result, id, "build-test", dir, false, &out); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	out.Reset()
	err := applyProposedSkill(result, id, "build-test", dir, false, &out)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second apply without --force should error, got %v", err)
	}

	out.Reset()
	if err := applyProposedSkill(result, id, "build-test", dir, true, &out); err != nil {
		t.Errorf("apply with --force should succeed, got %v", err)
	}
}

// TestProposeApply_RefusesWhenScriptExists: even if SKILL.md
// doesn't exist, an existing script under <skill>/scripts/<name>
// should block the write. The check fires before any file is
// created so the user doesn't end up with a half-applied skill.
func TestProposeApply_RefusesWhenScriptExists(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), s, id)

	dir := t.TempDir()
	scriptDir := filepath.Join(dir, "build-test", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "run-checks.sh"), []byte("# pre-existing\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	err := applyProposedSkill(result, id, "build-test", dir, false, &out)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("apply should refuse when script exists, got %v", err)
	}
	// SKILL.md must not have been created either.
	if _, err := os.Stat(filepath.Join(dir, "build-test", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("SKILL.md should not have been written when script blocked: %v", err)
	}
}

// TestFindProposedSkill_PrefixMatch covers the typo-friendly
// fallback: a unique prefix should resolve, an ambiguous one
// must error with the candidate list.
func TestFindProposedSkill_PrefixMatch(t *testing.T) {
	t.Parallel()
	r := sampleProposal()
	got, err := findProposedSkill(r, "build")
	if err != nil {
		t.Fatalf("unique prefix: %v", err)
	}
	if got.Name != "build-test" {
		t.Errorf("got %q, want build-test", got.Name)
	}

	r.Skills = append(r.Skills, prompts.ProposedSkill{Name: "build-bench"})
	if _, err := findProposedSkill(r, "build"); err == nil {
		t.Error("ambiguous prefix should error")
	}
}

func TestLoadLatestProposal_NoCachedRowIsError(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	_, _, err := loadLatestProposal(context.Background(), s, 0)
	if err == nil || !strings.Contains(err.Error(), "no cached propose") {
		t.Errorf("want 'no cached propose' error, got %v", err)
	}
}

// TestLoadLatestProposal_WrongKindIsError pins that --output-id
// pointing at a row whose kind != propose surfaces a clear error
// rather than silently producing a malformed ProposalResult.
func TestLoadLatestProposal_WrongKindIsError(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		// session_id NULL: skips FK check (we just need any
		// non-propose row for this negative-path test).
		SessionID:   sql.NullString{},
		Kind:        store.LLMKindSummary,
		Model:       "x",
		PromptHash:  "wk-test",
		Body:        `{"topic":"t"}`,
		CreatedAtMs: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, _, err = loadLatestProposal(context.Background(), s, id)
	if err == nil || !strings.Contains(err.Error(), "not a propose row") {
		t.Errorf("want wrong-kind error, got %v", err)
	}
}

func TestRenderProposalIndex_ListsSkillsAndScripts(t *testing.T) {
	t.Parallel()
	r := sampleProposal()
	output := &store.LLMOutput{ID: 42, Model: "m", CreatedAtMs: time.Now().UnixMilli()}
	var buf bytes.Buffer
	renderProposalIndex(&buf, r, output)
	body := buf.String()
	for _, want := range []string{
		"propose output id=42",
		"Skills (2):",
		"build-test",
		"scripts=1",
		"└── scripts/run-checks.sh",
		"another-skill",
		"propose apply --skill",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q\n%s", want, body)
		}
	}
}

func TestRenderProposalIndex_EmptySkillsMessage(t *testing.T) {
	t.Parallel()
	output := &store.LLMOutput{ID: 1, Model: "m", CreatedAtMs: time.Now().UnixMilli()}
	var buf bytes.Buffer
	renderProposalIndex(&buf, &prompts.ProposalResult{}, output)
	if !strings.Contains(buf.String(), "(no skills in proposal)") {
		t.Errorf("expected empty-state line, got: %s", buf.String())
	}
}

// TestRenderSkillScriptScaffold_EmptyBodyTODO confirms the LLM
// can omit `body` and we fall back to a TODO stub rather than
// inventing an empty script.
func TestRenderSkillScriptScaffold_EmptyBodyTODO(t *testing.T) {
	t.Parallel()
	sk := &prompts.ProposedSkill{Name: "x"}
	sc := &prompts.ProposedSkillScript{Name: "stub.sh", Purpose: "stub purpose"}
	body := renderSkillScriptScaffold(sc, sk, 99)
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

// TestRenderSkillScaffold_FrontmatterMatchesClaudeCodeDocs pins
// the canonical SKILL.md format documented at
// https://code.claude.com/docs/en/skills:
//   - name is kebab-case (NOT title-case);
//   - description and when_to_use live in separate frontmatter
//     fields;
//   - the body has a single H1 + a "## Steps" section, and does
//     NOT carry invented "When to apply" / "Why" / "Pitfalls" /
//     "Verification" headers.
func TestRenderSkillScaffold_FrontmatterMatchesClaudeCodeDocs(t *testing.T) {
	t.Parallel()
	r := sampleProposal()
	body := renderSkillScaffold(&r.Skills[0], 7)

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

	// Negative assertions — these headers were in my earlier
	// invented format and must NOT appear in the canonical one.
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

// TestRenderSkillScaffold_ScriptReferencedInline confirms that
// when a skill has a script, it gets cited inline in Steps as
// `Run scripts/<name> to <purpose>` — matching the docs'
// "Reference supporting files from your SKILL.md" convention.
func TestRenderSkillScaffold_ScriptReferencedInline(t *testing.T) {
	t.Parallel()
	r := sampleProposal()
	body := renderSkillScaffold(&r.Skills[0], 7)
	want := "Run `scripts/run-checks.sh` to run gofmt + vet + tests + race in one shot"
	if !strings.Contains(body, want) {
		t.Errorf("scaffold should reference the script inline:\nwant substring: %s\n--- body ---\n%s",
			want, body)
	}
}

// TestRenderSkillScaffold_FrontmatterParsesAsValidYAML confirms
// the yaml.v3-marshalled frontmatter survives a round-trip and
// carries the expected fields. This is the tested invariant
// behind the user's "use a yaml module" feedback — frontmatter
// generation no longer relies on hand-rolled quoting.
func TestRenderSkillScaffold_FrontmatterParsesAsValidYAML(t *testing.T) {
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
	body := renderSkillScaffold(&sk, 1)

	// Extract frontmatter between the two --- fences.
	fm, _, ok := strings.Cut(strings.TrimPrefix(body, "---\n"), "---\n")
	if !ok {
		t.Fatalf("could not find closing --- in:\n%s", body)
	}

	var parsed skillFrontmatter
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

func TestClipToRunes_LandsOnWordBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "short string passes through", in: "hello", max: 100, want: "hello"},
		{name: "clip at word boundary", in: "hello world how are you", max: 12,
			want: "hello world…"},
		{name: "no boundary near end", in: "supercalifragilisticexpialidocious", max: 10,
			want: "supercalif…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clipToRunes(tc.in, tc.max); got != tc.want {
				t.Errorf("clipToRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestLowerFirst(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Hello world": "hello world",
		"hello world": "hello world",
		"":            "",
		"X":           "x",
	}
	for in, want := range cases {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}
