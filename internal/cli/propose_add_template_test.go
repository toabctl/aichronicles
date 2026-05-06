package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
)

// proposalWithStepsTemplate fabricates a ProposalResult whose
// single skill carries an AWM-style steps[] template + the
// matching placeholders[]. The applier should materialise this
// as a runnable bash script with a placeholder doc-block.
func proposalWithStepsTemplate() *prompts.ProposalResult {
	ev := []prompts.ProposalEvidence{
		{SessionID: "abc12345-aaaa-bbbb-cccc-dddddddddddd",
			Quote:        "started work on the OAuth refresh-token rotation",
			WhatHappened: "user kicked off a worktree-based feature branch"},
		{SessionID: "def67890-aaaa-bbbb-cccc-dddddddddddd",
			Quote:        "started work on the migrate-009 patch",
			WhatHappened: "same worktree-launch flow"},
	}
	return &prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{
			{
				Name:      "wt-launch",
				WhenToUse: "Starting a new feature/bugfix branch.",
				Why:       "Same 4-line worktree dance every time.",
				Evidence:  ev,
				Frequency: 2,
				Effort:    "small",
				Scripts: []prompts.ProposedSkillScript{
					{
						Name:    "wt-start.sh",
						Purpose: "Spin up a wt-<topic> worktree off main.",
						Steps: []prompts.ProposedScriptStep{
							{Cmd: "git fetch origin", Purpose: "refresh main"},
							{Cmd: "git worktree add ../wt-{topic-slug} -b {branch-name} origin/main",
								Purpose: "create the worktree"},
							{Cmd: "cd ../wt-{topic-slug}",
								Purpose: "move into the worktree"},
							{Cmd: "claude",
								Purpose: "launch claude in the new tree"},
						},
						Placeholders: []prompts.ProposedScriptPlaceholder{
							{Token: "topic-slug",
								Description: "kebab-case slug naming the work",
								Example:     "oauth-refresh-rotation"},
							{Token: "branch-name",
								Description: "git branch for the work, usually wt-<topic-slug>",
								Example:     "wt-oauth-refresh-rotation"},
						},
					},
				},
			},
		},
	}
}

func TestProposeAdd_StepsTemplateMaterialisesAsBashScript(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, proposalWithStepsTemplate())

	dir := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), apiForStore(t, s), 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	if output.ID != id {
		t.Fatalf("loaded id %d, want %d", output.ID, id)
	}

	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"wt-launch", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
	}

	scriptPath := filepath.Join(dir, "wt-launch", "scripts", "wt-start.sh")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	got := string(body)

	for _, want := range []string{
		"#!/usr/bin/env bash",
		"# Spin up a wt-<topic> worktree off main.",
		"# Skill: wt-launch",
		// Placeholder block must list each {token} with its
		// description and example before any executable lines.
		"# Placeholders (substitute before running):",
		"#   {topic-slug} — kebab-case slug naming the work  e.g. oauth-refresh-rotation",
		"#   {branch-name} — git branch for the work, usually wt-<topic-slug>  e.g. wt-oauth-refresh-rotation",
		// Each step's purpose lands as a comment before the cmd.
		"# refresh main",
		"git fetch origin",
		"# create the worktree",
		"git worktree add ../wt-{topic-slug} -b {branch-name} origin/main",
		"# move into the worktree",
		"cd ../wt-{topic-slug}",
		"# launch claude in the new tree",
		"claude",
		"set -euo pipefail",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q\n--- script ---\n%s", want, got)
		}
	}

	// Placeholder block must precede the executable lines so the
	// reader sees the substitution instructions FIRST.
	placeholderIdx := strings.Index(got, "Placeholders (substitute")
	gitFetchIdx := strings.Index(got, "git fetch origin")
	if placeholderIdx < 0 || gitFetchIdx < 0 {
		t.Fatalf("anchor lines missing in:\n%s", got)
	}
	if placeholderIdx >= gitFetchIdx {
		t.Errorf("placeholder block should come BEFORE the steps; got placeholder@%d step@%d",
			placeholderIdx, gitFetchIdx)
	}

	// Script must be executable (mode 0755 from the applier).
	st, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("script not executable: mode=%v", st.Mode())
	}
}

func TestProposeAdd_StepsAndBodyAreMutuallyExclusive_StepsWin(t *testing.T) {
	t.Parallel()
	// When both Steps and Body are set, Steps wins. The schema
	// allows both at the JSON level (so the LLM doesn't fail to
	// parse on a borderline case) but the applier picks Steps so
	// the AWM-style template is never silently ignored.
	r := proposalWithStepsTemplate()
	r.Skills[0].Scripts[0].Body = "echo 'this body should not appear in the script'"

	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, r)
	dir := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), apiForStore(t, s), 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	if output.ID != id {
		t.Fatalf("loaded id %d, want %d", output.ID, id)
	}

	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"wt-launch", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "wt-launch", "scripts", "wt-start.sh"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)
	if strings.Contains(got, "this body should not appear") {
		t.Errorf("Body leaked into script when Steps was present:\n%s", got)
	}
	if !strings.Contains(got, "git worktree add") {
		t.Errorf("Steps content missing:\n%s", got)
	}
}

func TestProposeAdd_NoStepsNoBodyEmitsTODO(t *testing.T) {
	t.Parallel()
	r := proposalWithStepsTemplate()
	r.Skills[0].Scripts[0].Steps = nil
	r.Skills[0].Scripts[0].Body = ""

	s := openTempCLIStore(t)
	id := seedProposalOutput(t, s, r)
	dir := t.TempDir()
	result, output, err := loadLatestProposal(t.Context(), apiForStore(t, s), 0)
	if err != nil {
		t.Fatalf("loadLatestProposal: %v", err)
	}
	if output.ID != id {
		t.Fatalf("loaded id %d, want %d", output.ID, id)
	}

	var out bytes.Buffer
	if err := addSkillCandidate(t.Context(), s, apiForStore(t, s), result, output.ID, output.CreatedAtMs,
		"wt-launch", dir, false, true, nilLLMClient, &out); err != nil {
		t.Fatalf("addSkillCandidate: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "wt-launch", "scripts", "wt-start.sh"))
	if !strings.Contains(string(body), "TODO") {
		t.Errorf("expected TODO stub when Steps + Body are both empty:\n%s", body)
	}
}
