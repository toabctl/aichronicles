package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// seedProposalRow writes a synthetic kind=propose row to llm_outputs.
// Returns the row id so caller assertions can target the apply
// command's --output-id N tail.
func seedProposalRow(t *testing.T, st *store.Store, result prompts.ProposalResult, createdAtMs int64) int64 {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		// Propose outputs are multi-session — session_id NULL.
		SessionID:   sql.NullString{},
		Kind:        store.LLMKindPropose,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "h-" + strings.Join(skillNames(result), "-"),
		Body:        string(body),
		CreatedAtMs: createdAtMs,
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

func skillNames(r prompts.ProposalResult) []string {
	out := make([]string, 0, len(r.Skills))
	for _, s := range r.Skills {
		out = append(out, s.Name)
	}
	return out
}

func TestProposePage_RendersHeaderAndEmptyState(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/propose")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	for _, want := range []string{
		"Proposed skills",
		"No cached proposals yet",
		"aichronicles propose",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/propose missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestProposePage_RendersStoredCardWithApplyCmd(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	id := seedProposalRow(t, st, prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{
			Name:      "test-creation",
			WhenToUse: "Adding tests for the new web handlers.",
			Why:       "Recurring across 3 sessions, reduces toil.",
			Frequency: 3,
			Effort:    "small",
			Scripts: []prompts.ProposedSkillScript{
				{Name: "scaffold-test.sh", Purpose: "writes a Go test stub"},
			},
			Evidence: []prompts.ProposalEvidence{
				{SessionID: "11111111-2222-3333-4444-555555555555",
					Quote:        "fix the test for handler_skills",
					WhatHappened: "added missing assertion"},
			},
			AlternativesRejected: "skipping the test — leaves regression risk",
		}},
	}, now.UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/propose")
	for _, want := range []string{
		// Header
		"Proposal #",
		"claude-sonnet-4-6",
		// Skill body
		"<code>test-creation</code>",
		"×3",
		"effort: small",
		"Adding tests for the new web handlers.",
		"Recurring across 3 sessions",
		"scaffold-test.sh",
		"writes a Go test stub",
		// Evidence rendering
		`href="/sessions/11111111-2222-3333-4444-555555555555"`,
		"11111111",
		"fix the test for handler_skills",
		"Rejected alternatives:",
		// The copy-cmd button — must include --output-id <id>
		// so the copy-paste survives newer propose runs.
		"data-copy=\"aichronicles propose apply --skill test-creation --output-id ",
		"copy-btn",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("propose card missing %q\n%s", want, body)
		}
	}
	// Apply command must reference the ACTUAL row id we just seeded,
	// not the latest in the table.
	wantCmd := "aichronicles propose apply --skill test-creation --output-id "
	if !strings.Contains(body, wantCmd) {
		t.Errorf("expected apply command, got body:\n%s", body)
	}
	if !strings.Contains(body, "--output-id "+itoa(id)) {
		t.Errorf("apply command missing --output-id %d:\n%s", id, body)
	}
}

func TestProposePage_NewestFirst(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	older := prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{Name: "older-marker-skill", Frequency: 1}},
	}
	newer := prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{{Name: "newer-marker-skill", Frequency: 1}},
	}
	seedProposalRow(t, st, older, time.Date(2026, 4, 14, 6, 0, 0, 0, time.UTC).UnixMilli())
	seedProposalRow(t, st, newer, time.Date(2026, 4, 21, 6, 0, 0, 0, time.UTC).UnixMilli())

	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/propose")
	newerIdx := strings.Index(body, "newer-marker-skill")
	olderIdx := strings.Index(body, "older-marker-skill")
	if newerIdx < 0 || olderIdx < 0 {
		t.Fatalf("both markers should appear; newerIdx=%d olderIdx=%d", newerIdx, olderIdx)
	}
	if newerIdx >= olderIdx {
		t.Errorf("newer proposal should appear before older; newer=%d older=%d", newerIdx, olderIdx)
	}
}

func TestProposePage_RawBodyFallback(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)

	tx, _ := st.DB().Begin()
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:        store.LLMKindPropose,
		Model:       "old-model",
		PromptHash:  "bad",
		Body:        "this isn't json at all { malformed",
		CreatedAtMs: time.Date(2026, 4, 21, 6, 0, 0, 0, time.UTC).UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("save: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/propose")
	if status != http.StatusOK {
		t.Errorf("page should still render; got %d", status)
	}
	if !strings.Contains(body, "unparseable proposal") {
		t.Errorf("expected raw-body fallback, got:\n%s", body)
	}
	if !strings.Contains(body, "this isn&#39;t json at all") {
		t.Errorf("raw body should be HTML-escaped + surfaced, got:\n%s", body)
	}
}

func TestProposePage_RespectsLimitParam(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	for i := range 5 {
		// Vary the prompt_hash via skill name so SaveLLMOutput
		// dedup doesn't collapse the inserts to one row.
		seedProposalRow(t, st, prompts.ProposalResult{
			Skills: []prompts.ProposedSkill{{
				Name: "skill-" + string(rune('a'+i)), Frequency: 1,
			}},
		}, time.Date(2026, 4, 14, i, 0, 0, 0, time.UTC).UnixMilli())
	}
	base, stop := startTestServer(t, st)
	defer stop()

	_, full := fetch(t, base+"/propose")
	if got := strings.Count(full, "Proposal #"); got < 5 {
		t.Errorf("default limit should show all 5; got %d", got)
	}
	_, narrow := fetch(t, base+"/propose?limit=2")
	if got := strings.Count(narrow, "Proposal #"); got != 2 {
		t.Errorf("limit=2 should show 2; got %d", got)
	}
}

// itoa avoids pulling in strconv just for one call site that
// builds a substring assertion.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
