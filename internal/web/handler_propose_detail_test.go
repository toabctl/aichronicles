package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
)

// TestProposeDetail_RendersSkillMdPreview confirms /propose/{id}/{skill}
// renders the exact SKILL.md `propose add` would write: kebab-case
// frontmatter, the TODO-stubbed Steps section, the provenance
// footer, the copy-able add command, and a preview of each helper
// script.
func TestProposeDetail_RendersSkillMdPreview(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	id := seedProposalRow(t, st, prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{
			Name:      "build-test",
			WhenToUse: "Use when building and testing a Go service.",
			Why:       "Recurring across sessions.",
			Frequency: 3,
			Effort:    "medium",
			Scripts: []prompts.ProposedSkillScript{
				{Name: "run-checks.sh", Purpose: "Run gofmt + vet + tests.",
					Body: "go vet ./...\ngo test ./..."},
			},
		}},
	}, now.UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/propose/"+itoa(id)+"/build-test")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200\n%s", status, body)
	}
	for _, want := range []string{
		// Page chrome.
		"Preview:",
		"<code>build-test</code>",
		// The rendered SKILL.md, escaped inside <pre>.
		"name: build-test",
		"## Steps",
		"**TODO**",
		"aichronicles-provenance: sha256:",
		// Copy-able add command keyed to this row.
		"aichronicles propose add --skill build-test --output-id " + itoa(id),
		// Helper-script preview.
		"scripts/run-checks.sh",
		"#!/usr/bin/env bash",
		"go test ./...",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestProposeDetail_GroundsEvidence pins the anti-fabrication
// guarantee: evidence whose SessionID resolves to a real session is
// rendered in the SKILL.md footer; an unresolvable (hallucinated)
// SessionID is dropped, so the preview never cites a session the
// user can't open. Matches cli.loadLatestProposal's grounding.
func TestProposeDetail_GroundsEvidence(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	realID := seedSession(t, st, "sess-real", "build the thing", now)
	const fakeID = "deadbeef-0000-0000-0000-000000000000"

	id := seedProposalRow(t, st, prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{
			Name:      "grounded-skill",
			WhenToUse: "x",
			Why:       "y",
			Frequency: 2,
			Evidence: []prompts.ProposalEvidence{
				{SessionID: realID, Quote: "q1", WhatHappened: "did real work"},
				{SessionID: fakeID, Quote: "q2", WhatHappened: "hallucinated work"},
			},
		}},
	}, now.UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/propose/"+itoa(id)+"/grounded-skill")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	if !strings.Contains(body, realID) {
		t.Errorf("preview should cite the resolvable session %q\n%s", realID, body)
	}
	if strings.Contains(body, fakeID) {
		t.Errorf("preview must drop the unresolvable session %q (grounding failed)\n%s", fakeID, body)
	}
}

func TestProposeDetail_NotFound(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedProposalRow(t, st, prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{Name: "real-skill", Frequency: 1}},
	}, now.UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	cases := map[string]string{
		"unknown skill in a real proposal": base + "/propose/" + itoa(id) + "/no-such-skill",
		"unknown output id":                base + "/propose/999999/real-skill",
		"non-numeric id":                   base + "/propose/abc/real-skill",
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			status, _ := fetch(t, url)
			if status != http.StatusNotFound {
				t.Errorf("%s: got %d, want 404", url, status)
			}
		})
	}
}

// TestProposePage_LinksToDetail confirms the recent-runs cards link
// each skill name to its /propose/{id}/{skill} preview.
func TestProposePage_LinksToDetail(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	id := seedProposalRow(t, st, prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{Name: "linked-skill", Frequency: 1}},
	}, now.UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/propose")
	want := `href="/propose/` + itoa(id) + `/linked-skill"`
	if !strings.Contains(body, want) {
		t.Errorf("propose page should link to the detail preview %q\n%s", want, body)
	}
}
