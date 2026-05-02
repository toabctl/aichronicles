package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

func TestResolveWeekBounds_DefaultsToPreviousMonday(t *testing.T) {
	t.Parallel()
	// Now = Wednesday 2026-04-29. Previous completed Monday is
	// 2026-04-20 → next Monday 2026-04-27.
	now := time.Date(2026, 4, 29, 14, 30, 0, 0, time.UTC)
	start, end, err := resolveWeekBounds("", now)
	if err != nil {
		t.Fatalf("resolveWeekBounds: %v", err)
	}
	if got, want := start.Format("2006-01-02 Mon"), "2026-04-20 Mon"; got != want {
		t.Errorf("start: got %q, want %q", got, want)
	}
	if got, want := end.Format("2006-01-02 Mon"), "2026-04-27 Mon"; got != want {
		t.Errorf("end: got %q, want %q", got, want)
	}
}

func TestResolveWeekBounds_OnMondayReturnsPriorWeek(t *testing.T) {
	t.Parallel()
	// Edge case: now IS a Monday. The "previous completed week"
	// must NOT be the week that just started — it's the prior
	// week (Mon-Mon).
	now := time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC) // Monday
	start, end, err := resolveWeekBounds("", now)
	if err != nil {
		t.Fatalf("resolveWeekBounds: %v", err)
	}
	if got, want := start.Format("2006-01-02"), "2026-04-20"; got != want {
		t.Errorf("start: got %q, want %q", got, want)
	}
	if got, want := end.Format("2006-01-02"), "2026-04-27"; got != want {
		t.Errorf("end: got %q, want %q", got, want)
	}
}

func TestResolveWeekBounds_WeekOfArgSnapsToMonday(t *testing.T) {
	t.Parallel()
	// Pass a Wednesday inside week-of-2026-04-20; expect snap to Mon.
	start, end, err := resolveWeekBounds("2026-04-22", time.Now().UTC())
	if err != nil {
		t.Fatalf("resolveWeekBounds: %v", err)
	}
	if got := start.Format("2006-01-02 Mon"); got != "2026-04-20 Mon" {
		t.Errorf("start: got %q, want 2026-04-20 Mon", got)
	}
	if got := end.Format("2006-01-02 Mon"); got != "2026-04-27 Mon" {
		t.Errorf("end: got %q, want 2026-04-27 Mon", got)
	}
}

func TestResolveWeekBounds_BadDateIsError(t *testing.T) {
	t.Parallel()
	if _, _, err := resolveWeekBounds("nope", time.Now()); err == nil {
		t.Error("bad --week-of should error")
	}
}

func TestDigestHashFor_DifferentWeeksDifferentHashes(t *testing.T) {
	t.Parallel()
	w1Start := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	w2Start := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	a := digestHashFor("base", w1Start, w1Start.Add(week))
	b := digestHashFor("base", w2Start, w2Start.Add(week))
	if a == b {
		t.Errorf("two different weeks should produce different hashes: %q == %q", a, b)
	}
}

// seedSessionWithSummary inserts a session + its first user_prompt
// + a cached summary so the reflect path has substantive material
// (digestsFromRowsWithLinks rejects sessions without a summary).
func seedSessionWithSummary(t *testing.T, s *store.Store, sourceID, prompt string, ts time.Time) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceID,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts,
		Cwd:             "/work/" + sourceID,
		ContentText:     prompt,
		Payload:         map[string]any{},
		Redaction:       &events.Redaction{Applied: true},
	}
	tx, _ := s.DB().Begin()
	if _, err := store.IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	sid := events.DeriveSessionID("claude-code", sourceID)
	body, _ := json.Marshal(prompts.SummaryResult{
		Topic:       "topic for " + sourceID,
		WhatWasDone: []string{"work A", "work B"},
	})
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sqlNullStr(sid),
		Kind:        store.LLMKindSummary,
		Model:       "test-model",
		PromptHash:  "h-" + sourceID,
		Body:        string(body),
		CreatedAtMs: ts.UnixMilli(),
	}); err != nil {
		t.Fatalf("save summary: %v", err)
	}
	_ = tx.Commit()
}

func sqlNullStr(s string) (n struct {
	String string
	Valid  bool
}) {
	return struct {
		String string
		Valid  bool
	}{String: s, Valid: s != ""}
}

// TestRunDigestWeekly_PersistsArtefactWithCorrectKind covers the
// load-bearing invariant: the row lands as kind=reflect_weekly
// (NOT plain reflect), so `digest list` and the future web
// timeline find it.
func TestRunDigestWeekly_PersistsArtefactWithCorrectKind(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	// Two sessions inside the target week, both with summaries.
	weekStart := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	weekEnd := weekStart.AddDate(0, 0, 7)
	seedSessionWithSummary(t, s, "wk-1", "investigate slow plan",
		weekStart.Add(2*time.Hour))
	seedSessionWithSummary(t, s, "wk-2", "compare libvirt against tumbleweed",
		weekStart.Add(48*time.Hour))

	f := &fakeLLM{reply: "use the planner more"}
	var buf bytes.Buffer
	id, err := RunDigestWeekly(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		DigestWeeklyOptions{
			PeriodStart: weekStart,
			PeriodEnd:   weekEnd,
			Limit:       25,
		},
		&buf)
	if err != nil {
		t.Fatalf("RunDigestWeekly: %v", err)
	}
	if id == 0 {
		t.Fatal("expected a non-zero llm_outputs id")
	}
	if f.called != 1 {
		t.Errorf("LLM call count: got %d, want 1", f.called)
	}

	var kind string
	if err := s.DB().QueryRow(
		`SELECT kind FROM llm_outputs WHERE id = ?`, id,
	).Scan(&kind); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kind != string(store.LLMKindReflectWeekly) {
		t.Errorf("kind: got %q, want reflect_weekly", kind)
	}

	body := buf.String()
	if !strings.Contains(body, "weekly digest") {
		t.Errorf("rendered output missing header:\n%s", body)
	}
	if !strings.Contains(body, "2026-04-20") {
		t.Errorf("rendered output missing period start:\n%s", body)
	}
}

// TestRunDigestWeekly_NoSessionsIsError pins the empty-window
// failure mode: a clean error rather than a confusing zero-LLM
// pass.
func TestRunDigestWeekly_NoSessionsIsError(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	weekStart := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	weekEnd := weekStart.AddDate(0, 0, 7)

	f := &fakeLLM{reply: "should not be reached"}
	_, err := RunDigestWeekly(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		DigestWeeklyOptions{
			PeriodStart: weekStart,
			PeriodEnd:   weekEnd,
		}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no sessions") {
		t.Errorf("want 'no sessions' error, got %v", err)
	}
	if f.called != 0 {
		t.Errorf("LLM should not be called for empty week, count=%d", f.called)
	}
}

// TestRunDigestWeekly_FiltersToWindow ensures sessions that ended
// AFTER PeriodEnd don't bleed into the digest. Reflect's loader
// is open-ended on the upper side, so the digest path applies the
// upper bound itself.
func TestRunDigestWeekly_FiltersToWindow(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)

	weekStart := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	weekEnd := weekStart.AddDate(0, 0, 7)

	// Two in-window sessions (reflect needs ≥2 with summaries).
	seedSessionWithSummary(t, s, "in-w-1", "first work",
		weekStart.Add(48*time.Hour))
	seedSessionWithSummary(t, s, "in-w-2", "second work",
		weekStart.Add(72*time.Hour))
	// Out-of-window: ends two weeks LATER. Must not influence
	// the digest.
	seedSessionWithSummary(t, s, "after-w", "future work",
		weekEnd.Add(7*24*time.Hour))

	f := &fakeLLM{reply: "ok"}
	if _, err := RunDigestWeekly(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		DigestWeeklyOptions{PeriodStart: weekStart, PeriodEnd: weekEnd, Limit: 25},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("RunDigestWeekly: %v", err)
	}
	prompt := f.lastReq.Messages[0].Content
	if !strings.Contains(prompt, "first work") {
		t.Errorf("in-window session should be in prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "future work") {
		t.Errorf("out-of-window session leaked into prompt:\n%s", prompt)
	}
}

// TestRunDigestWeekly_RerunSameWeekHitsCache confirms idempotency:
// a second run for the same week doesn't call the LLM again.
func TestRunDigestWeekly_RerunSameWeekHitsCache(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	weekStart := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	weekEnd := weekStart.AddDate(0, 0, 7)
	seedSessionWithSummary(t, s, "rerun-1", "do thing", weekStart.Add(time.Hour))
	seedSessionWithSummary(t, s, "rerun-2", "do another thing", weekStart.Add(2*time.Hour))

	f := &fakeLLM{reply: "first"}
	for i := 0; i < 3; i++ {
		_, err := RunDigestWeekly(context.Background(), s,
			func() (llm.Client, error) { return f, nil },
			DigestWeeklyOptions{PeriodStart: weekStart, PeriodEnd: weekEnd, Limit: 25},
			&bytes.Buffer{})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if f.called != 1 {
		t.Errorf("rerun should hit cache: LLM called %d times, want 1", f.called)
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"hello":                    "hello",
		"hello world this is long": "hello world…",
	}
	for in, want := range cases {
		if got := truncateRunes(in, 12); got != want {
			t.Errorf("truncateRunes(%q, 12) = %q, want %q", in, got, want)
		}
	}
}
