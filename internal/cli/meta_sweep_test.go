package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// countLLMOutputsByKind is a small assertion helper.
func countLLMOutputsByKind(t *testing.T, s *store.Store, kind store.LLMOutputKind) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM llm_outputs WHERE kind = ?`, string(kind)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

// TestRunMetaAnalysisSweep_FiresOverdueKinds confirms a fresh store
// (no prior meta-analysis rows) triggers every non-skipped kind on
// the first sweep. The skill_revision path is the exception — with
// no stale skills in the staleness report, it has nothing to do
// and exits cleanly without producing a row.
func TestRunMetaAnalysisSweep_FiresOverdueKinds(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	f := &fakeLLM{reply: "ok"}
	newClient := func() (llm.Client, error) { return f, nil }

	err := RunMetaAnalysisSweep(t.Context(), s, newClient,
		MetaAnalysisSweepOptions{
			ProposeCadence:       24 * time.Hour,
			ProposeSinceWindow:   10 * time.Hour,
			ProposeLimit:         10,
			ReflectCadence:       24 * time.Hour,
			ReflectSinceWindow:   10 * time.Hour,
			ReflectLimit:         10,
			ChallengeCadence:     24 * time.Hour,
			ChallengeSinceWindow: 10 * time.Hour,
			ChallengeLimit:       10,
			ReflectWeeklyCadence: 24 * time.Hour,
			SkillRevisionCadence: 24 * time.Hour,
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("RunMetaAnalysisSweep: %v", err)
	}

	for _, kind := range []store.LLMOutputKind{
		store.LLMKindPropose,
		store.LLMKindReflect,
		store.LLMKindChallenge,
	} {
		if got := countLLMOutputsByKind(t, s, kind); got != 1 {
			t.Errorf("%s: expected 1 row after sweep, got %d", kind, got)
		}
	}
	// reflect_weekly: 4 sessions all created "now"-ish so they may
	// or may not fall into the previous-completed-week window.
	// Either 0 or 1 rows is acceptable; the assertion that
	// matters is that the sweep didn't error.
}

// TestRunMetaAnalysisSweep_SkipFlagsBypassDispatch confirms that
// every per-kind Skip flag short-circuits without consulting the
// cadence — even a far-overdue kind stays untouched.
func TestRunMetaAnalysisSweep_SkipFlagsBypassDispatch(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	f := &fakeLLM{reply: "ok"}
	newClient := func() (llm.Client, error) { return f, nil }

	err := RunMetaAnalysisSweep(t.Context(), s, newClient,
		MetaAnalysisSweepOptions{
			ProposeCadence: 24 * time.Hour, ProposeSkip: true,
			ReflectCadence: 24 * time.Hour, ReflectSkip: true,
			ChallengeCadence: 24 * time.Hour, ChallengeSkip: true,
			ReflectWeeklyCadence: 24 * time.Hour, ReflectWeeklySkip: true,
			SkillRevisionCadence: 24 * time.Hour, SkillRevisionSkip: true,
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("RunMetaAnalysisSweep: %v", err)
	}
	if f.called != 0 {
		t.Errorf("expected 0 LLM calls with all kinds skipped, got %d", f.called)
	}
}

// TestRunMetaAnalysisSweep_ZeroCadenceDisablesKind confirms that
// a zero-or-negative cadence makes a kind a no-op (defensive: the
// daemon's main.go always supplies a default, but the orchestrator
// must not assume that).
func TestRunMetaAnalysisSweep_ZeroCadenceDisablesKind(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	f := &fakeLLM{reply: "ok"}
	newClient := func() (llm.Client, error) { return f, nil }

	err := RunMetaAnalysisSweep(t.Context(), s, newClient,
		MetaAnalysisSweepOptions{
			// Only propose has a positive cadence.
			ProposeCadence:     24 * time.Hour,
			ProposeSinceWindow: 10 * time.Hour,
			ProposeLimit:       10,
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("RunMetaAnalysisSweep: %v", err)
	}
	if got := countLLMOutputsByKind(t, s, store.LLMKindPropose); got != 1 {
		t.Errorf("propose: got %d rows, want 1", got)
	}
	if got := countLLMOutputsByKind(t, s, store.LLMKindReflect); got != 0 {
		t.Errorf("reflect: got %d rows, want 0 (zero cadence should disable)", got)
	}
}

// TestRunMetaAnalysisSweep_RecentRowSkipsKind confirms the cadence
// gate consults the persisted timestamp. After a propose row is
// already present, a sweep with cadence > age must NOT re-fire.
func TestRunMetaAnalysisSweep_RecentRowSkipsKind(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	f := &fakeLLM{reply: "ok"}
	newClient := func() (llm.Client, error) { return f, nil }

	if _, err := RunPropose(t.Context(), s, newClient,
		ProposeOptions{Since: 10 * time.Hour, Limit: 10},
		&bytes.Buffer{}); err != nil {
		t.Fatalf("seed propose: %v", err)
	}
	calls := f.called
	err := RunMetaAnalysisSweep(t.Context(), s, newClient,
		MetaAnalysisSweepOptions{
			ProposeCadence:     24 * time.Hour,
			ProposeSinceWindow: 10 * time.Hour,
			ProposeLimit:       10,
			// Other kinds disabled.
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("RunMetaAnalysisSweep: %v", err)
	}
	if f.called != calls {
		t.Errorf("expected sweep to skip propose (recent row), but LLM was called again (calls=%d→%d)", calls, f.called)
	}
}

// TestRunMetaAnalysisSweep_PerKindFailureIsolation confirms one
// kind's failure doesn't prevent later kinds from running. We
// inject a propose failure (via empty window) and assert reflect
// still produces a row.
func TestRunMetaAnalysisSweep_PerKindFailureIsolation(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	// fakeLLM that returns a transient error on the FIRST call (for
	// propose) and succeeds afterwards. propose is dispatched first;
	// reflect runs second.
	f := &flakeyLLM{firstErr: errors.New("transient propose failure"), reply: "ok"}
	newClient := func() (llm.Client, error) { return f, nil }

	err := RunMetaAnalysisSweep(t.Context(), s, newClient,
		MetaAnalysisSweepOptions{
			ProposeCadence:     24 * time.Hour,
			ProposeSinceWindow: 10 * time.Hour,
			ProposeLimit:       10,
			ReflectCadence:     24 * time.Hour,
			ReflectSinceWindow: 10 * time.Hour,
			ReflectLimit:       10,
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("expected an error from the propose failure")
	}
	if !strings.Contains(err.Error(), "transient propose failure") {
		t.Errorf("expected propose error to surface; got %v", err)
	}
	// Despite the propose failure, reflect should still have run
	// and produced a row.
	if got := countLLMOutputsByKind(t, s, store.LLMKindReflect); got != 1 {
		t.Errorf("reflect: expected 1 row despite propose failure, got %d", got)
	}
	if got := countLLMOutputsByKind(t, s, store.LLMKindPropose); got != 0 {
		t.Errorf("propose: expected 0 rows after failure, got %d", got)
	}
}

// TestRunMetaAnalysisSweep_EmptyWindowIsNotAFailure confirms the
// "no sessions in window" path is treated as quiet (returns nil),
// not as a sweep error. Otherwise a fresh install with no captured
// sessions would page the operator.
func TestRunMetaAnalysisSweep_EmptyWindowIsNotAFailure(t *testing.T) {
	t.Parallel()
	s := testStore(t) // empty store
	f := &fakeLLM{reply: "ok"}
	newClient := func() (llm.Client, error) { return f, nil }

	err := RunMetaAnalysisSweep(t.Context(), s, newClient,
		MetaAnalysisSweepOptions{
			ProposeCadence:     24 * time.Hour,
			ProposeSinceWindow: 10 * time.Hour,
			ProposeLimit:       10,
			ReflectCadence:     24 * time.Hour,
			ReflectSinceWindow: 10 * time.Hour,
			ReflectLimit:       10,
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("empty store should not produce sweep error: %v", err)
	}
	if f.called != 0 {
		t.Errorf("expected 0 LLM calls (no sessions to feed prompts), got %d", f.called)
	}
}

// TestIsEmptyWindowErr exercises the message-matching gate.
func TestIsEmptyWindowErr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("propose: no sessions in the requested window"), true},
		{errors.New("digest weekly: no sessions in week of 2026-04-20"), true},
		{errors.New("digest weekly: no summarised sessions in week of 2026-04-20"), true},
		{errors.New("some unrelated failure"), false},
		{context.Canceled, false},
	} {
		if got := isEmptyWindowErr(tc.err); got != tc.want {
			t.Errorf("isEmptyWindowErr(%v): got %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestRunMetaAnalysisSweep_CtxCancelStopsSweep ensures a cancelled
// context unwinds promptly rather than running every kind.
func TestRunMetaAnalysisSweep_CtxCancelStopsSweep(t *testing.T) {
	t.Parallel()
	s := seedSessionsForMeta(t, 4)
	newClient := func() (llm.Client, error) { return &fakeLLM{reply: "ok"}, nil }

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancelled before we even start

	err := RunMetaAnalysisSweep(ctx, s, newClient,
		MetaAnalysisSweepOptions{
			ProposeCadence:     24 * time.Hour,
			ProposeSinceWindow: 10 * time.Hour,
			ProposeLimit:       10,
			ReflectCadence:     24 * time.Hour,
			ReflectSinceWindow: 10 * time.Hour,
			ReflectLimit:       10,
		},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	// Either the ctx error surfaces directly (a kind tried to run
	// and failed at the DB layer with context.Canceled) or we
	// return cleanly because the cadence-check loop bails out at
	// the post-kind ctx check. Both are valid; what matters is
	// that no rows landed.
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("expected nil or context.Canceled, got %v", err)
	}
	if got := countLLMOutputsByKind(t, s, store.LLMKindPropose); got != 0 {
		t.Errorf("propose: expected 0 rows on cancelled ctx, got %d", got)
	}
	if got := countLLMOutputsByKind(t, s, store.LLMKindReflect); got != 0 {
		t.Errorf("reflect: expected 0 rows on cancelled ctx, got %d", got)
	}
}

// flakeyLLM returns firstErr on the first Complete() call and
// succeeds (with `reply`) on every subsequent call. Used to test
// per-kind failure isolation in the meta-sweep.
type flakeyLLM struct {
	firstErr error
	reply    string
	calls    int
}

func (f *flakeyLLM) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	f.calls++
	if f.calls == 1 && f.firstErr != nil {
		return nil, f.firstErr
	}
	// Delegate to fakeLLM-equivalent shape for the success path.
	resp := &llm.Response{
		Model: "claude-sonnet-4-6",
		Usage: llm.Usage{InputTokens: 17, OutputTokens: 23},
	}
	if req.ForceTool == "" {
		resp.Text = f.reply
		return resp, nil
	}
	resp.ToolUses = []llm.ToolUse{{
		ID:    "toolu_flakey",
		Name:  req.ForceTool,
		Input: synthMinimalToolInput(req.ForceTool, f.reply),
	}}
	return resp, nil
}
