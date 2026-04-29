package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		Rationale: "extracted the concrete deploy recipe",
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
		Skill:     nil,
		Workflow:  nil,
		Rationale: "single one-off bug fix; not a reusable workflow",
	}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	if _, err := RunInductionForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionRunOptions{SessionID: sessID}, &out); err != nil {
		t.Fatalf("RunInductionForSession: %v", err)
	}
	if !strings.Contains(out.String(), "nothing reusable") {
		t.Errorf("output should announce nothing-reusable verdict:\n%s", out.String())
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
		Rationale: "nothing reusable",
	}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	var out, errOut bytes.Buffer
	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		// Skip facts + workflow for this test; their behaviour is
		// covered by dedicated tests below. Leaving them on would
		// triple the LLM call count and conflate the assertions.
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SkipFacts: true,
		},
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
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SkipFacts: true,
		},
		&bytes.Buffer{}, &errOut); err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	if f2.called != 0 {
		t.Errorf("idempotency broken: re-sweep called LLM %d times", f2.called)
	}
}

// TestRunInductionSweep_DefaultAutoExtractsAllMemoryTypes confirms
// that with default options (SkipFacts=false) one candidate
// triggers two LLM calls: the unified induction call (which can
// emit skill+workflow inline) and a separate facts induction.
// Round 8 collapsed the previous three-call shape (induction +
// workflow + facts) by merging skill + workflow into one prompt.
func TestRunInductionSweep_DefaultAutoExtractsAllMemoryTypes(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "investigation",
		"investigated thoroughly across multiple services and components")

	// fakeLLM returns the same body for every call. The body
	// happens to be a minimal InductionResult; for the facts call
	// the json.Unmarshal will populate a zero-valued FactsResult.
	// That's fine — we're asserting CALL COUNT and persistence
	// side-effects, not body content.
	indResult := prompts.InductionResult{
		Rationale: "nothing reusable",
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
	if f.called != 2 {
		t.Errorf("default auto-extract should trigger 2 LLM calls per candidate (merged induction + facts), got %d",
			f.called)
	}

	// Verify persistence: one row each at induction / facts kinds.
	// (workflow is now embedded inside the induction body, not its
	// own row.)
	for _, kind := range []store.LLMOutputKind{
		store.LLMKindInduction, store.LLMKindFacts,
	} {
		var n int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM llm_outputs WHERE session_id = ? AND kind = ?`,
			sessID, string(kind),
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", kind, err)
		}
		if n != 1 {
			t.Errorf("expected 1 llm_outputs row of kind=%s after sweep, got %d", kind, n)
		}
	}
}

// TestRunInductionSweep_SkipFactsSuppressesFactsLayer: the
// SkipFacts opt-out is honoured per-candidate.
func TestRunInductionSweep_SkipFactsSuppressesFactsLayer(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "investigation",
		"investigated thoroughly across multiple services and components")

	indResult := prompts.InductionResult{Rationale: "x"}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SkipFacts: true,
		},
		&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if f.called != 1 {
		t.Errorf("expected 1 LLM call (merged induction only — facts skipped, workflow inline), got %d", f.called)
	}
	// No facts row should exist.
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM llm_outputs WHERE session_id = ? AND kind = 'facts'`, sessID,
	).Scan(&n); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	if n != 0 {
		t.Errorf("SkipFacts violated: %d facts rows persisted", n)
	}
}

// TestRunInductionSweep_AutoSummarizesSessionsWithoutSummary: when
// the candidate session has no kind=summary row yet, phase 1 fires
// RunSummarize before phases 2+3 — the autonomous pipeline.
// Expected total LLM calls per candidate: 3 (summarize + induction
// + facts). A fakeLLM that returns the same toolInput for every call
// is fine because we're asserting CALL COUNT and persistence
// side-effects, not body content (the body is wrong-shaped for
// most of the calls but unmarshalls into a zero-valued result —
// also fine for this test).
func TestRunInductionSweep_AutoSummarizesSessionsWithoutSummary(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	// NOTE: deliberately do NOT plantSummary — phase 1 is the
	// thing under test.

	// Generic toolInput that's accepted by every kind's parse path
	// (the JSON unmarshals into zero-valued result structs for the
	// kinds that don't share fields). The test only asserts call
	// count + row persistence.
	indResult := prompts.InductionResult{Rationale: "nothing reusable"}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	var out, errOut bytes.Buffer
	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&out, &errOut); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if f.called != 3 {
		t.Errorf("expected 3 LLM calls (summarize + induction + facts), got %d", f.called)
	}

	// Verify all three kinds landed.
	for _, kind := range []store.LLMOutputKind{
		store.LLMKindSummary, store.LLMKindInduction, store.LLMKindFacts,
	} {
		var n int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM llm_outputs WHERE session_id = ? AND kind = ?`,
			sessID, string(kind),
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", kind, err)
		}
		if n != 1 {
			t.Errorf("expected 1 llm_outputs row of kind=%s after sweep, got %d", kind, n)
		}
	}
}

// TestRunInductionSweep_AlreadySummarizedSessionSkipsPhase1: a
// session with an existing kind=summary row should NOT trigger
// RunSummarize — the existence check elides phase 1. Existing
// tests rely on this (they plantSummary), but this test makes the
// invariant explicit.
func TestRunInductionSweep_AlreadySummarizedSessionSkipsPhase1(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "already summarized",
		"summary already present, phase 1 should detect and skip")

	indResult := prompts.InductionResult{Rationale: "x"}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if f.called != 2 {
		t.Errorf("expected 2 LLM calls (induction + facts only — phase 1 cache-skipped), got %d", f.called)
	}
}

// TestRunInductionSweep_SkipSummarizeBypassesPhase1: with
// SkipSummarize=true and a session lacking a summary, phase 1
// is entirely suppressed. Phases 2+3 then bail with their
// existing "no summary" error — neither calls the LLM.
func TestRunInductionSweep_SkipSummarizeBypassesPhase1(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	// No summary planted, SkipSummarize=true → phases 2+3 hit
	// their "no summary" guard and return without an LLM call.

	f := &fakeLLM{reply: "should not be reached"}

	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SkipSummarize: true,
		},
		&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if f.called != 0 {
		t.Errorf("expected 0 LLM calls (SkipSummarize + no summary → all phases bail), got %d", f.called)
	}
	// No rows of any LLM kind should exist for this session.
	for _, kind := range []store.LLMOutputKind{
		store.LLMKindSummary, store.LLMKindInduction, store.LLMKindFacts,
	} {
		var n int
		_ = s.DB().QueryRow(
			`SELECT COUNT(*) FROM llm_outputs WHERE session_id = ? AND kind = ?`,
			sessID, string(kind),
		).Scan(&n)
		if n != 0 {
			t.Errorf("expected 0 rows of kind=%s, got %d", kind, n)
		}
	}
}

// TestRunInductionSweep_SummarizeFailureSkipsDownstream: when
// phase 1 fails, phases 2+3 must NOT fire for the same session
// (they both gate on summary). fakeLLM that errors on every call;
// expected total calls = 1 (summarize attempt only). Per-session
// failure isolation: the sweep itself returns nil (logs +
// continues).
func TestRunInductionSweep_SummarizeFailureSkipsDownstream(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	// No summary planted, default options — phase 1 fires, errors.

	f := &fakeLLM{err: errors.New("simulated LLM outage")}

	var errOut bytes.Buffer
	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&bytes.Buffer{}, &errOut); err != nil {
		t.Fatalf("sweep should not return on per-session failure: %v", err)
	}
	if f.called != 1 {
		t.Errorf("expected 1 LLM call (summarize fails first, downstream skipped), got %d", f.called)
	}
	// Stderr should mention summarize failure specifically — that's
	// the contract: phase-1 failure is distinguishable from later
	// failures.
	body := errOut.String()
	if !strings.Contains(body, "summarize") {
		t.Errorf("expected stderr to log summarize failure, got:\n%s", body)
	}
	// Sanity: no rows persisted for any kind on this session.
	for _, kind := range []store.LLMOutputKind{
		store.LLMKindSummary, store.LLMKindInduction, store.LLMKindFacts,
	} {
		var n int
		_ = s.DB().QueryRow(
			`SELECT COUNT(*) FROM llm_outputs WHERE session_id = ? AND kind = ?`,
			sessID, string(kind),
		).Scan(&n)
		if n != 0 {
			t.Errorf("expected 0 rows of kind=%s after summarize failure, got %d", kind, n)
		}
	}
	_ = sessID
}

// TestRunInductionSweep_FactsLayerErrorDoesNotAbortSweep: a per-
// candidate facts failure must NOT stop the sweep — the user's
// other induction work proceeds.
func TestRunInductionSweep_FactsLayerErrorDoesNotAbortSweep(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "investigation",
		"investigated thoroughly across multiple services and components")

	// fakeLLM that errors on every call — induction and facts both
	// fail. The sweep must complete without panic.
	f := &fakeLLM{err: errors.New("simulated LLM outage")}

	var errOut bytes.Buffer
	err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{Idle: 30 * time.Minute, MinEvents: 5, Limit: 10},
		&bytes.Buffer{}, &errOut)
	if err != nil {
		t.Fatalf("sweep should not return error on per-session failure: %v", err)
	}
	body := errOut.String()
	if !strings.Contains(body, "facts ") {
		t.Errorf("expected stderr to log facts failure, got:\n%s", body)
	}
	// Induction failure should also be logged (the per-session
	// "✗ <id>: ..." line) — the merged call's failure is the same
	// path as before.
	if !strings.Contains(body, "induction: LLM call") {
		t.Errorf("expected stderr to log induction failure, got:\n%s", body)
	}
}

// TestRunInductionSweep_PerPhaseTimeoutsAreIndependent confirms the
// fix for the session-9ec75b11 bug: a per-call timeout no longer
// depletes the budget for subsequent calls, because each phase gets
// its OWN bounded context derived from the parent. Pre-fix, all
// calls shared one parent ctx with a single deadline, so a slow
// summarize on candidate 1 would strangle every call on candidates
// 2..N with "context deadline exceeded".
//
// We verify the fix by capturing each call's ctx deadline; before
// the fix every deadline equalled the same wall-clock instant
// (parent's). After the fix each deadline reflects a fresh per-
// phase budget.
func TestRunInductionSweep_PerPhaseTimeoutsAreIndependent(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	// Make the session look idle and substantial.
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "x", "did stuff across services and components")

	f := &deadlineCapturingLLM{
		// Provide a schema-valid induction reply so phase 2 doesn't
		// fail on decode (we want every phase to land an LLM call,
		// not bail on a parse error).
		toolInput: json.RawMessage(mustJSON(t, prompts.InductionResult{Rationale: "noop"})),
	}

	const summarizeTO = 100 * time.Millisecond
	const inductionTO = 200 * time.Millisecond
	const factsTO = 50 * time.Millisecond

	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SummarizeTimeout: summarizeTO,
			InductionTimeout: inductionTO,
			FactsTimeout:     factsTO,
		},
		&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}

	// The session has a planted summary, so phase 1 is skipped via
	// the HasLLMOutputForSession cache check. Phases 2 and 3 each
	// land one LLM call.
	if got := len(f.deadlines); got != 2 {
		t.Fatalf("expected 2 LLM calls (induction + facts), got %d", got)
	}

	// Each captured deadline should reflect ITS phase's budget —
	// roughly time.Now() + per-phase timeout. We allow generous
	// slack since wall clocks aren't perfect; the assertion that
	// matters is that the deadlines are NEAR the expected per-call
	// budget (proves per-call WithTimeout) and NOT something like
	// "300ms in the past" (which is what shared-budget mode would
	// produce after the first call burns the parent budget).
	for i, d := range f.deadlines {
		remaining := time.Until(d)
		if remaining <= 0 {
			t.Errorf("call %d: deadline already past (%s) — looks like a shared parent budget", i, d)
		}
		// Each call should see a deadline roughly within the
		// per-phase timeout we configured. The widest budget
		// here is inductionTO=200ms; double it for slack.
		if remaining > 2*inductionTO {
			t.Errorf("call %d: deadline %s is suspiciously far in the future — expected per-phase budget", i, remaining)
		}
	}
}

// deadlineCapturingLLM records the context deadline of each
// Complete() call. Used to assert that per-phase WithTimeout
// budgets are applied independently, not inherited from a
// shared parent context.
type deadlineCapturingLLM struct {
	toolInput json.RawMessage
	deadlines []time.Time
}

func (d *deadlineCapturingLLM) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.deadlines = append(d.deadlines, dl)
	}
	resp := &llm.Response{
		Model: "claude-sonnet-4-6",
		Usage: llm.Usage{InputTokens: 1, OutputTokens: 1},
	}
	if req.ForceTool == "" {
		resp.Text = "ok"
		return resp, nil
	}
	input := d.toolInput
	if input == nil {
		input = synthMinimalToolInput(req.ForceTool, "ok")
	}
	resp.ToolUses = []llm.ToolUse{{
		ID:    "toolu_dl",
		Name:  req.ForceTool,
		Input: input,
	}}
	return resp, nil
}

// countEpisodes returns how many rows exist in the episodes table
// for the given session. Used by the episode-phase tests below.
func countEpisodes(t *testing.T, s *store.Store, sessionID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM episodes WHERE session_id = ?`, sessionID,
	).Scan(&n); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	return n
}

// TestRunInductionSweep_SegmentsEpisodesByDefault asserts that the
// daemon-resident sweep populates the episodes table for every
// candidate it processes. Phase 0 is local-only (no LLM call) so
// this works without a fakeLLM result; we plant a summary so
// phases 1+ don't bail early and confound the assertion.
func TestRunInductionSweep_SegmentsEpisodesByDefault(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "x", "did a thing across multiple services and components")

	indResult := prompts.InductionResult{Rationale: "x"}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SkipFacts: true,
		},
		&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	// The seed fixture's 4 events all share /work/systemd cwd and
	// sit within seconds of each other → the segmenter produces
	// exactly one episode covering the whole session.
	if got := countEpisodes(t, s, sessID); got != 1 {
		t.Errorf("expected 1 episode persisted, got %d", got)
	}
}

// TestRunInductionSweep_SkipEpisodesSuppressesPhase0: opt-out is
// honoured.
func TestRunInductionSweep_SkipEpisodesSuppressesPhase0(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "x", "did a thing across multiple services and components")

	indResult := prompts.InductionResult{Rationale: "x"}
	toolInput, _ := json.Marshal(indResult)
	f := &fakeLLM{toolInput: toolInput}

	if err := RunInductionSweep(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		InductionSweepOptions{
			Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
			SkipEpisodes: true, SkipFacts: true,
		},
		&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunInductionSweep: %v", err)
	}
	if got := countEpisodes(t, s, sessID); got != 0 {
		t.Errorf("SkipEpisodes violated: %d episode rows persisted", got)
	}
}

// TestRunInductionSweep_EpisodeSegmentationIsIdempotent: re-running
// the sweep on the same candidate (forced by clearing the induction
// row so the candidate query re-selects it) produces the same
// episode set. SaveEpisodes is DELETE-then-INSERT so the row count
// stays stable and ordinal/start-time round-trip.
func TestRunInductionSweep_EpisodeSegmentationIsIdempotent(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	oneHourAgo := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := s.DB().Exec(
		`UPDATE sessions SET ended_at_ms = ?, event_count = 30 WHERE id = ?`,
		oneHourAgo, sessID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	plantSummary(t, s, sessID, "x", "did a thing across multiple services and components")

	indResult := prompts.InductionResult{Rationale: "x"}
	toolInput, _ := json.Marshal(indResult)

	run := func() {
		f := &fakeLLM{toolInput: toolInput}
		if err := RunInductionSweep(context.Background(), s,
			func() (llm.Client, error) { return f, nil },
			InductionSweepOptions{
				Idle: 30 * time.Minute, MinEvents: 5, Limit: 10,
				SkipFacts: true,
			},
			&bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("RunInductionSweep: %v", err)
		}
	}
	run()
	first := countEpisodes(t, s, sessID)
	if first != 1 {
		t.Fatalf("first sweep: expected 1 episode, got %d", first)
	}

	// Drop the induction row so the candidate query re-selects this
	// session and phase 0 fires again. Without this, the candidate
	// filter would skip the already-induced session.
	if _, err := s.DB().Exec(
		`DELETE FROM llm_outputs WHERE session_id = ? AND kind = 'induction'`, sessID,
	); err != nil {
		t.Fatalf("clear induction row: %v", err)
	}
	run()
	if got := countEpisodes(t, s, sessID); got != first {
		t.Errorf("idempotency broken: got %d episodes after re-sweep, want %d", got, first)
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
