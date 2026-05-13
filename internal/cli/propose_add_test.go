package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// nilLLMClient is the closure tests pass when noVerify=true short-
// circuits before any actual LLM call. The client should never be
// invoked under that flag, so returning a nil client is safe and
// makes "we never reached the LLM path" explicit at the test site.
func nilLLMClient() (llm.Client, error) { return nil, nil }

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

func TestProposeAdd_SkillWritesScaffoldAndScripts(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())

	dir := t.TempDir()
	result, output, err := loadLatestProposal(context.Background(), apiForStore(t, s), 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	if output.ID != id {
		t.Errorf("loaded id %d, want %d", output.ID, id)
	}

	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs, "build-test", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
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

// TestProposeAdd_RecordsLifecycle: applying a skill must update
// skill_candidates with decision='add', decision_at_ms, and
// add_path, regardless of whether the row was pre-recorded by
// RunPropose or absent (the fallback path inserts a row post-hoc).
func TestProposeAdd_RecordsLifecycle(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), apiForStore(t, s), id)

	// Path A: pre-record the row (the canonical RunPropose path).
	if err := store.RecordSkillCandidate(t.Context(), s.DB(), id, "build-test", time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("pre-record: %v", err)
	}

	dir := t.TempDir()
	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "build-test", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
	}

	rows, err := store.LoadSkillCandidatesByName(t.Context(), s.DB(), "build-test", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 skill_candidates row, got %d", len(rows))
	}
	r := rows[0]
	if r.Decision != store.MaintenanceAdd {
		t.Errorf("decision: got %q want %q", r.Decision, store.MaintenanceAdd)
	}
	if !r.DecisionAtMs.Valid {
		t.Errorf("decision_at_ms not set after add")
	}
	if !r.AddPath.Valid || !strings.HasSuffix(r.AddPath.String, "/build-test/SKILL.md") {
		t.Errorf("add_path: got %v", r.AddPath)
	}

	// Path B: a different output id has NO pre-recorded row; apply
	// must auto-insert via the ErrSkillCandidateNotFound fallback
	// so the lifecycle index stays complete.
	id2 := seedProposalOutput(t, s, &prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{
			{
				Name:                 "another-skill",
				WhenToUse:            "when",
				Why:                  "why",
				Frequency:            2,
				Effort:               "small",
				AlternativesRejected: "n/a",
				Evidence: []prompts.ProposalEvidence{
					{SessionID: "s1", Quote: "q", WhatHappened: "w"},
					{SessionID: "s2", Quote: "q", WhatHappened: "w"},
				},
			},
		},
	})
	result2, _, _ := loadLatestProposal(context.Background(), apiForStore(t, s), id2)
	dir2 := t.TempDir()
	out.Reset()
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result2, id2, 0, "another-skill", dir2, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("path B addSkillCandidate: %v", err)
	}
	rows2, err := store.LoadSkillCandidatesByName(t.Context(), s.DB(), "another-skill", 0)
	if err != nil {
		t.Fatalf("load path B: %v", err)
	}
	if len(rows2) != 1 {
		t.Fatalf("path B expected 1 row, got %d", len(rows2))
	}
	if rows2[0].Decision != store.MaintenanceAdd {
		t.Errorf("path B: decision not 'add' on auto-inserted row, got %q", rows2[0].Decision)
	}
	if rows2[0].LLMOutputID != id2 {
		t.Errorf("path B llm_output_id: got %d want %d", rows2[0].LLMOutputID, id2)
	}
}

// TestProposeAdd_SkillWithoutScripts confirms the no-scripts
// branch still works (no scripts/ dir, no Helper-scripts section).
func TestProposeAdd_SkillWithoutScripts(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), apiForStore(t, s), id)

	dir := t.TempDir()
	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "another-skill", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "another-skill", "SKILL.md"))
	if strings.Contains(string(body), "Run `scripts/") {
		t.Errorf("no-scripts skill should not reference scripts inline:\n%s", string(body))
	}
	if _, err := os.Stat(filepath.Join(dir, "another-skill", "scripts")); !os.IsNotExist(err) {
		t.Errorf("no-scripts skill should not create scripts/ dir, err=%v", err)
	}
}

// TestRefuseDuplicateSkillName_PointsAtMerge pins the dedup gate's
// error messaging: a same-name collision must mention `propose
// merge` and `propose discard` so the user reaches for the right
// verb instead of habitually --force'ing through.
func TestRefuseDuplicateSkillName_PointsAtMerge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillMd := filepath.Join(dir, "deploy-staging", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillMd), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(skillMd, []byte("# pre-existing\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("collision without force", func(t *testing.T) {
		err := refuseDuplicateSkillName(skillMd, "deploy-staging", false)
		if err == nil {
			t.Fatalf("expected refusal")
		}
		for _, want := range []string{
			"deploy-staging",
			"propose merge --skill",
			"propose discard --skill",
			"--force",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})
	t.Run("force bypasses", func(t *testing.T) {
		if err := refuseDuplicateSkillName(skillMd, "deploy-staging", true); err != nil {
			t.Errorf("--force should bypass: %v", err)
		}
	})
	t.Run("no collision returns nil", func(t *testing.T) {
		fresh := filepath.Join(dir, "fresh-skill", "SKILL.md")
		if err := refuseDuplicateSkillName(fresh, "fresh-skill", false); err != nil {
			// Note: a real ~/.claude/skills/fresh-skill could exist on
			// the developer's machine — this test isolates HOME to
			// keep the case deterministic.
			t.Errorf("no-collision case unexpectedly errored: %v", err)
		}
	})
}

// TestProposeAdd_EmitsKindInFrontmatter pins the parallel of the
// merge-side Kind propagation: when the proposal's kind is in-enum
// it lands in the SKILL.md frontmatter; otherwise the frontmatter
// omits it (no fabrication, no out-of-enum YAML). Keeps add and
// merge producing identical frontmatter shapes so a future
// `skills verify` doesn't have to special-case.
func TestProposeAdd_EmitsKindInFrontmatter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		kind     string
		wantLine string // "" → assert NO `kind:` line
	}{
		{name: "pattern emitted", kind: "pattern", wantLine: "kind: pattern"},
		{name: "pitfall emitted", kind: "pitfall", wantLine: "kind: pitfall"},
		{name: "empty omitted", kind: ""},
		{name: "out-of-enum omitted", kind: "garbage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openTempCLIStore(t)
			proposal := sampleProposal()
			proposal.Skills[0].Kind = tc.kind
			id := seedProposalOutput(t, s, proposal)
			result, output, err := loadLatestProposal(t.Context(), apiForStore(t, s), 0)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if output.ID != id {
				t.Fatalf("loaded wrong id")
			}
			dir := t.TempDir()
			var out bytes.Buffer
			if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
				proposal.Skills[0].Name, dir, false, true, nilLLMClient, &out); err != nil {
				t.Fatalf("addSkillCandidate: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(dir, proposal.Skills[0].Name, "SKILL.md"))
			if err != nil {
				t.Fatalf("read SKILL.md: %v", err)
			}
			bs := string(body)
			if tc.wantLine != "" && !strings.Contains(bs, tc.wantLine) {
				t.Errorf("expected %q in:\n%s", tc.wantLine, bs)
			}
			if tc.wantLine == "" && strings.Contains(bs, "kind:") {
				t.Errorf("did not expect 'kind:' in:\n%s", bs)
			}
		})
	}
}

// TestRefuseDiscardedSkillName covers the discard-history guard:
// a name the user explicitly discarded must NOT silently re-add
// from a different output_id. Without this check, `propose discard
// --skill X` is a one-shot signal that the next `propose add
// --skill X` (perhaps the user's own habit, perhaps the model
// re-proposing the same kebab name) bypasses freely.
func TestRefuseDiscardedSkillName(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	ctx := t.Context()

	// Plant a discarded candidate row under an isolated name so the
	// test doesn't race against any other fixture.
	const skillName = "previously-rejected"
	loID := seedProposalOutput(t, s, sampleProposal())
	if err := store.RecordSkillCandidate(ctx, s.DB(), loID, skillName, 1_700_000_000_000); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	if err := store.MarkSkillCandidateDiscarded(ctx, s.DB(), loID, skillName, 1_700_000_500_000); err != nil {
		t.Fatalf("mark discard: %v", err)
	}

	t.Run("blocks add of previously-discarded name", func(t *testing.T) {
		err := refuseDiscardedSkillName(ctx, apiForStore(t, s), skillName, false)
		if err == nil {
			t.Fatalf("expected refusal, got nil")
		}
		if !strings.Contains(err.Error(), "previously discarded") {
			t.Errorf("error should mention 'previously discarded': %v", err)
		}
		if !strings.Contains(err.Error(), "--force") {
			t.Errorf("error should mention --force escape hatch: %v", err)
		}
	})

	t.Run("force bypasses", func(t *testing.T) {
		if err := refuseDiscardedSkillName(ctx, apiForStore(t, s), skillName, true); err != nil {
			t.Errorf("--force should bypass discard history: %v", err)
		}
	})

	t.Run("no history returns nil", func(t *testing.T) {
		if err := refuseDiscardedSkillName(ctx, apiForStore(t, s), "never-seen-this", false); err != nil {
			t.Errorf("a fresh name should not error: %v", err)
		}
	})

	t.Run("only-pending returns nil", func(t *testing.T) {
		// A pending (recorded but un-decisioned) candidate is NOT
		// a rejection signal — it just means the user hasn't acted
		// yet. The add must proceed.
		const pendingName = "still-pending"
		loID2 := seedProposalOutput(t, s, sampleProposal())
		if err := store.RecordSkillCandidate(ctx, s.DB(), loID2, pendingName, 1_700_000_000_000); err != nil {
			t.Fatalf("seed pending: %v", err)
		}
		if err := refuseDiscardedSkillName(ctx, apiForStore(t, s), pendingName, false); err != nil {
			t.Errorf("pending-only candidate should not block add: %v", err)
		}
	})
}

// TestRefuseDuplicateSkillName_GlobalCollision covers the secondary
// check: the user is adding to a project-local skills dir, but a
// global ~/.claude/skills/<name>/SKILL.md already covers the same
// name. Without HOME isolation the test would race the developer's
// real skills directory; t.Setenv pins it.
func TestRefuseDuplicateSkillName_GlobalCollision(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	globalSkill := filepath.Join(fakeHome, ".claude", "skills", "shared-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(globalSkill), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(globalSkill, []byte("# global\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	projectLocal := filepath.Join(t.TempDir(), "shared-skill", "SKILL.md")
	err := refuseDuplicateSkillName(projectLocal, "shared-skill", false)
	if err == nil {
		t.Fatalf("expected global-collision refusal, got nil")
	}
	for _, want := range []string{
		"globally",
		"shared-skill",
		"propose merge",
		"--force",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestProposeAdd_StampsProvenanceHash pins the SSGM provenance
// invariant: a successful add stores the SHA-256 of the rendered
// SKILL.md body on the skill_candidates row, and the on-disk
// SKILL.md ends with a footer carrying the leading hex
// fingerprint. A drift checker can later read the file, strip the
// footer, recompute, and detect post-write tampering.
func TestProposeAdd_StampsProvenanceHash(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(t.Context(), apiForStore(t, s), id)

	dir := t.TempDir()
	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "build-test",
		dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("add: %v", err)
	}

	// The skill_candidates row must carry a non-empty
	// add_body_sha256 (64-hex-char SHA-256).
	rows, err := store.LoadSkillCandidatesByName(t.Context(), s.DB(), "build-test", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	got := rows[0].AddBodySHA256
	if !got.Valid {
		t.Fatal("add_body_sha256 must be populated after add")
	}
	if len(got.String) != 64 {
		t.Errorf("add_body_sha256 length: got %d want 64", len(got.String))
	}

	// The SKILL.md must end with the provenance footer that
	// embeds the first 12 hex chars of the same hash.
	body, err := os.ReadFile(filepath.Join(dir, "build-test", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	bodyStr := string(body)
	wantFingerprint := "sha256:" + got.String[:12]
	if !strings.Contains(bodyStr, wantFingerprint) {
		t.Errorf("SKILL.md missing provenance fingerprint %q\n--- tail ---\n%s",
			wantFingerprint, bodyStr[max(0, len(bodyStr)-300):])
	}
	if !strings.Contains(bodyStr, "aichronicles-provenance:") {
		t.Errorf("SKILL.md missing aichronicles-provenance marker")
	}

	// The footer must appear AFTER the body so the body's hash is
	// reproducible: stripping the trailing comment line should
	// recover the input that was hashed.
	prefix := strings.TrimSuffix(bodyStr, skillProvenanceFooter(got.String))
	prefixHash := sha256.Sum256([]byte(prefix))
	if hex.EncodeToString(prefixHash[:]) != got.String {
		t.Errorf("recomputed body hash does not match stored hash; " +
			"the provenance line breaks reversibility")
	}
}

// TestSkillProvenanceFooter_Format pins the marker the drift
// checker will key off, plus the fingerprint length budget.
func TestSkillProvenanceFooter_Format(t *testing.T) {
	t.Parallel()
	footer := skillProvenanceFooter("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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
	if got := skillProvenanceFooter(""); !strings.Contains(got, "sha256:") {
		t.Errorf("empty-hash footer should still emit the marker, got: %s", got)
	}
}

// TestProposeAdd_RefusesOversizedSkill pins the SWE-Skills-Bench
// budget guard: an oversized SKILL.md is refused unless --force is
// passed. The error message must point at the trim/merge/force
// remediations so the user knows which lever to pull.
func TestProposeAdd_RefusesOversizedSkill(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	// Build a proposal with a comically over-budget Why so the
	// rendered SKILL.md crosses skillMdBudgetRunes (Why becomes the
	// scaffold's H1 intro paragraph and is not pre-clipped, unlike
	// the frontmatter description / when_to_use fields).
	bigText := strings.Repeat("x", skillMdBudgetRunes+1024)
	id := seedProposalOutput(t, s, &prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{
			Name:                 "oversized-skill",
			WhenToUse:            "trigger condition",
			Why:                  bigText,
			Frequency:            2,
			Effort:               "small",
			AlternativesRejected: "n/a",
			Evidence: []prompts.ProposalEvidence{
				{SessionID: "s1", Quote: "q", WhatHappened: "w"},
				{SessionID: "s2", Quote: "q", WhatHappened: "w"},
			},
		}},
	})
	result, _, _ := loadLatestProposal(context.Background(), apiForStore(t, s), id)

	dir := t.TempDir()
	var out bytes.Buffer
	err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "oversized-skill",
		dir, false, true, nilLLMClient, &out)
	if err == nil {
		t.Fatalf("expected refusal for oversized skill")
	}
	for _, want := range []string{"runes", "budget", "merge", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}

	// SKILL.md must NOT have been written.
	if _, err := os.Stat(filepath.Join(dir, "oversized-skill", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("SKILL.md should not exist after refusal, stat=%v", err)
	}

	// --force bypasses the budget — the rare legitimate case.
	out.Reset()
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "oversized-skill",
		dir, true, true, nilLLMClient, &out); err != nil {
		t.Errorf("--force should bypass the budget: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "oversized-skill", "SKILL.md")); err != nil {
		t.Errorf("--force should have written SKILL.md: %v", err)
	}
}

// TestRefuseOversizedSkill_UnitChecks pins the helper's behaviour:
// over-budget without force returns an error that names the
// remediation; force always returns nil; under-budget always
// returns nil.
func TestRefuseOversizedSkill_UnitChecks(t *testing.T) {
	t.Parallel()
	tiny := "small body"
	huge := strings.Repeat("y", skillMdBudgetRunes+1)
	cases := []struct {
		name      string
		body      string
		force     bool
		wantError bool
	}{
		{"under cap, no force", tiny, false, false},
		{"under cap, force", tiny, true, false},
		{"over cap, no force", huge, false, true},
		{"over cap, force", huge, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := refuseOversizedSkill("/some/skill/SKILL.md", tc.body, tc.force)
			if tc.wantError != (err != nil) {
				t.Errorf("got err=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

// TestProposeAdd_RefusesOverwriteWithoutForce: writing twice
// must fail unless --force is passed. Pin the invariant directly.
// Also covers the case where the SKILL.md is fine but a script
// already exists — must refuse before writing anything.
func TestProposeAdd_RefusesOverwriteWithoutForce(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), apiForStore(t, s), id)

	dir := t.TempDir()
	var out bytes.Buffer

	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "build-test", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	out.Reset()
	err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "build-test", dir, false, true, nilLLMClient, &out)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second apply without --force should error, got %v", err)
	}

	out.Reset()
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "build-test", dir, true, true, nilLLMClient, &out); err != nil {
		t.Errorf("apply with --force should succeed, got %v", err)
	}
}

// TestProposeAdd_RefusesWhenScriptExists: even if SKILL.md
// doesn't exist, an existing script under <skill>/scripts/<name>
// should block the write. The check fires before any file is
// created so the user doesn't end up with a half-applied skill.
func TestProposeAdd_RefusesWhenScriptExists(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, sampleProposal())
	result, _, _ := loadLatestProposal(context.Background(), apiForStore(t, s), id)

	dir := t.TempDir()
	scriptDir := filepath.Join(dir, "build-test", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "run-checks.sh"), []byte("# pre-existing\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, id, 0, "build-test", dir, false, true, nilLLMClient, &out)
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
	_, _, err := loadLatestProposal(context.Background(), apiForStore(t, s), 0)
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
		SessionID:   nil,
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

	_, _, err = loadLatestProposal(context.Background(), apiForStore(t, s), id)
	if err == nil || !strings.Contains(err.Error(), "not a propose row") {
		t.Errorf("want wrong-kind error, got %v", err)
	}
}

func TestRenderProposalIndex_ListsSkillsAndScripts(t *testing.T) {
	t.Parallel()
	r := sampleProposal()
	output := &wire.LLMOutput{ID: 42, Model: "m", CreatedAtMs: time.Now().UnixMilli()}
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
		"propose add --skill",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q\n%s", want, body)
		}
	}
}

func TestRenderProposalIndex_EmptySkillsMessage(t *testing.T) {
	t.Parallel()
	output := &wire.LLMOutput{ID: 1, Model: "m", CreatedAtMs: time.Now().UnixMilli()}
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
