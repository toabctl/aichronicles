package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// plantSummary writes a minimal kind='summary' row attributed to
// sessionID. Required for RunInductionForSession because the
// digest-from-rows path skips sessions without a summary.
func plantSummary(t *testing.T, s *store.Store, sessionID, topic, doneLine string) {
	t.Helper()
	body, _ := json.Marshal(prompts.SummaryResult{
		Topic:        topic,
		WhatWasDone:  []string{doneLine},
		Unresolved:   []string{},
		KeyFiles:     []string{},
		Links:        []prompts.LinkAnnotation{},
		Subagents:    []prompts.SubagentSummary{},
		SessionLinks: []prompts.SessionLinkAnnotation{},
	})
	if _, err := s.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
		 VALUES (?, 'summary', ?, 'h-'||?, 'fake-model', ?)`,
		sessionID, string(body), sessionID, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("plant summary: %v", err)
	}
}

func TestRunInductionForSession_RequiresSummary(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	// No summary planted.
	f := &fakeLLM{reply: "should not be called"}
	_, err := RunInductionForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionRunOptions{SessionID: sessID}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no summary") {
		t.Fatalf("expected 'no summary' error, got %v", err)
	}
	if f.called != 0 {
		t.Errorf("LLM should not be called when summary is missing, got %d calls", f.called)
	}
}

func TestRunInductionForSession_PersistsSkillProposal(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID,
		"deploy-staging walkthrough",
		"ran the staging deploy across two services and patched the config drift")

	indResult := prompts.InductionResult{
		Skill: &prompts.ProposedSkill{
			Name:      "deploy-staging",
			WhenToUse: "When deploying to staging across the two backend services",
			Why:       "two-step deploy procedure surfaced repeatedly",
			Evidence: []prompts.ProposalEvidence{{
				SessionID:    sessID,
				Quote:        "ran the staging deploy across two services and patched the config drift",
				WhatHappened: "the user followed the deploy-staging recipe",
			}},
			Frequency:            1,
			Effort:               "small",
			AlternativesRejected: "none",
		},
		NoSkillFound: false,
		Rationale:    "extracted the concrete deploy recipe",
	}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	id, err := RunInductionForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionRunOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunInductionForSession: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero row id")
	}

	row, err := store.LoadInductionRow(t.Context(), s.DB(), sessID)
	if err != nil {
		t.Fatalf("LoadInductionRow: %v", err)
	}
	if row == nil {
		t.Fatal("expected an induction row")
	}
	if !row.SessionID.Valid || row.SessionID.String != sessID {
		t.Errorf("row.session_id = %+v, want %s", row.SessionID, sessID)
	}
	if row.Kind != store.LLMKindInduction {
		t.Errorf("row.kind = %q, want %q", row.Kind, store.LLMKindInduction)
	}

	for _, want := range []string{"deploy-staging", "When deploying to staging"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunInductionForSession_HandlesNoSkillVerdict(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID,
		"one-off bug fix",
		"fixed an off-by-one in the date helper across one test")

	indResult := prompts.InductionResult{
		Skill:        nil,
		NoSkillFound: true,
		Rationale:    "single one-off bug fix; not a reusable workflow",
	}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	if _, err := RunInductionForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionRunOptions{SessionID: sessID}, &out); err != nil {
		t.Fatalf("RunInductionForSession: %v", err)
	}
	if !strings.Contains(out.String(), "no skill") {
		t.Errorf("output should announce no-skill verdict:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "single one-off bug fix") {
		t.Errorf("rationale missing from output:\n%s", out.String())
	}
}

func TestRunInductionSweep_ProcessesIdleSessionsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)

	// Force the session to look idle and substantial.
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "investigation",
		"investigated thoroughly across multiple services and components")

	indResult := prompts.InductionResult{
		NoSkillFound: true,
		Rationale:    "nothing reusable",
	}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	var out, errOut bytes.Buffer
	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&out, &errOut); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if f.called != 1 {
		t.Errorf("expected 1 LLM call, got %d", f.called)
	}
	if !strings.Contains(errOut.String(), "candidates=1") {
		t.Errorf("stderr missing candidate count:\n%s", errOut.String())
	}

	// Re-running must skip the already-induced session.
	f2 := &fakeLLM{toolInput: toolInput}
	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f2, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&bytes.Buffer{}, &errOut); err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	if f2.called != 0 {
		t.Errorf("idempotency broken: re-sweep called LLM %d times", f2.called)
	}
}

func TestRunInductionSweep_NoCandidatesReturnsCleanly(t *testing.T) {
	t.Parallel()
	s, _ := seedSessionForSummarize(t)
	// Don't update ended_at — session looks recent so it shouldn't
	// match the idle window.
	f := &fakeLLM{reply: "noop"}

	var out, errOut bytes.Buffer
	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&out, &errOut); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if f.called != 0 {
		t.Errorf("LLM should not be called when no candidates, got %d", f.called)
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("expected 'nothing to do' message:\n%s", out.String())
	}
}
