package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// llmOutputsToWire converts store rows to the api wire shape so the
// summaries-list renderer (which now consumes []wire.LLMOutput) can
// be exercised against fixtures seeded directly into the store.
func llmOutputsToWire(rows []store.LLMOutput) []wire.LLMOutput {
	out := make([]wire.LLMOutput, 0, len(rows))
	for _, r := range rows {
		o := wire.LLMOutput{
			ID:          r.ID,
			Kind:        string(r.Kind),
			Model:       r.Model,
			PromptHash:  r.PromptHash,
			Body:        r.Body,
			CreatedAtMs: r.CreatedAtMs,
		}
		o.SessionID = r.SessionID
		o.InputTokens = r.InputTokens
		o.OutputTokens = r.OutputTokens
		out = append(out, o)
	}
	return out
}

// seedSummariesFixtures writes one of each kind so the list/show
// commands have something to render. Returns the store and the full
// session id used for the `summary` row.
func seedSummariesFixtures(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := testStore(t)

	// One ingest-shaped event so the session exists for the summary
	// row's foreign key.
	env := events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sum-fixture",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"x": 1},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, _ := s.DB().Begin()
	if _, _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed event: %v", err)
	}
	_ = tx.Commit()
	sessID := events.DeriveSessionID("claude-code", "sum-fixture")

	// Store one summary, one reflection, one proposal. Body is the
	// proper JSON a real tool_use would produce so extractTopic
	// returns real labels rather than "(unparseable)".
	summary, _ := json.Marshal(prompts.SummaryResult{
		Topic:       "Refactor the ingest loop",
		WhatWasDone: []string{"did stuff"},
	})
	reflection, _ := json.Marshal(prompts.ReflectionResult{
		WorkflowChange: "Stop fixing errors one line at a time",
	})
	proposal, _ := json.Marshal(prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{
			{
				Name: "extract-metrics", WhenToUse: "w", Why: "y",
				Evidence: []prompts.ProposalEvidence{
					{SessionID: "s1", Quote: "q", WhatHappened: "h"},
					{SessionID: "s2", Quote: "q", WhatHappened: "h"},
				},
				Frequency: 2, Effort: "small", AlternativesRejected: "",
			},
		},
	})

	now := time.Now().UnixMilli()
	tx, _ = s.DB().Begin()
	defer func() { _ = tx.Commit() }()

	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(sessID),
		Kind:        store.LLMKindSummary,
		Model:       "claude",
		PromptHash:  "h-sum-1",
		Body:        string(summary),
		CreatedAtMs: now - 3000,
	}); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:        store.LLMKindReflect,
		Model:       "claude",
		PromptHash:  "h-ref-1",
		Body:        string(reflection),
		CreatedAtMs: now - 2000,
	}); err != nil {
		t.Fatalf("seed reflection: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:        store.LLMKindPropose,
		Model:       "claude",
		PromptHash:  "h-pro-1",
		Body:        string(proposal),
		CreatedAtMs: now - 1000,
	}); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}

	return s, sessID
}

func TestSummariesList_ShowsEveryKindWithTopic(t *testing.T) {
	t.Parallel()
	s, _ := seedSummariesFixtures(t)

	var out bytes.Buffer
	rows, err := store.LoadLLMOutputs(t.Context(), s.DB(), store.LLMOutputFilter{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := writeSummaries(&out, llmOutputsToWire(rows), FormatTable); err != nil {
		t.Fatalf("writeSummariesTable: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"ID", "KIND", "SESSION", "TOPIC",
		"summary", "reflect", "propose",
		"Refactor the ingest loop",
		"Stop fixing errors",
		"extract-metrics",
		"(multi)", // reflection + proposal have NULL session
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("list output missing %q:\n%s", want, rendered)
		}
	}
}

func TestSummariesList_EmptyResultGivesPlaceholder(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var out bytes.Buffer
	rows, _ := store.LoadLLMOutputs(t.Context(), s.DB(), store.LLMOutputFilter{})
	if err := writeSummaries(&out, llmOutputsToWire(rows), FormatTable); err != nil {
		t.Fatalf("writeSummariesTable: %v", err)
	}
	if !strings.Contains(out.String(), "(no outputs)") {
		t.Errorf("expected placeholder, got %q", out.String())
	}
}

func TestExtractTopic_FallsBackWhenBodyIsNotJSON(t *testing.T) {
	t.Parallel()
	// Legacy prose body — no schema. extractTopic must report
	// "(unparseable)" without panicking so old rows stay discoverable
	// by id in the listing.
	got := extractTopic(store.LLMKindSummary, "Topic: legacy prose\n\nWhat was done:\n- stuff")
	if got != "(unparseable)" {
		t.Errorf("legacy body: got %q, want (unparseable)", got)
	}
}

func TestExtractTopic_TruncatesLongTopic(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	body, _ := json.Marshal(prompts.SummaryResult{Topic: long})
	got := extractTopic(store.LLMKindSummary, string(body))
	if len(got) > 90 { // 80 + ellipsis + margin
		t.Errorf("not truncated: len=%d, got=%q", len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated topic should end with ellipsis, got %q", got)
	}
}

func TestExtractTopic_ProposalWithMultipleSkillsShowsCount(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(prompts.ProposalResult{
		Skills: []prompts.ProposedSkill{
			{
				Name: "first", WhenToUse: "w", Why: "y",
				Evidence: []prompts.ProposalEvidence{
					{SessionID: "s1", Quote: "q", WhatHappened: "h"},
					{SessionID: "s2", Quote: "q", WhatHappened: "h"},
				},
				Frequency: 2, Effort: "small",
			},
			{
				Name: "second", WhenToUse: "w", Why: "y",
				Evidence: []prompts.ProposalEvidence{
					{SessionID: "s1", Quote: "q", WhatHappened: "h"},
					{SessionID: "s2", Quote: "q", WhatHappened: "h"},
				},
				Frequency: 2, Effort: "small",
			},
			{
				Name: "third", WhenToUse: "w", Why: "y",
				Evidence: []prompts.ProposalEvidence{
					{SessionID: "s1", Quote: "q", WhatHappened: "h"},
					{SessionID: "s2", Quote: "q", WhatHappened: "h"},
				},
				Frequency: 2, Effort: "small",
			},
		},
	})
	got := extractTopic(store.LLMKindPropose, string(body))
	if !strings.Contains(got, "first") {
		t.Errorf("first skill missing: %q", got)
	}
	if !strings.Contains(got, "+2 more") {
		t.Errorf("should note additional skills, got %q", got)
	}
}

func TestParseOutputKind_AcceptsAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want store.LLMOutputKind
	}{
		{"summary", store.LLMKindSummary},
		{"SUMMARY", store.LLMKindSummary},
		{"reflect", store.LLMKindReflect},
		{"reflection", store.LLMKindReflect},
		{"propose", store.LLMKindPropose},
		{"proposal", store.LLMKindPropose},
	}
	for _, tc := range cases {
		got, err := parseOutputKind(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := parseOutputKind("nonsense"); err == nil {
		t.Error("expected error for unknown kind")
	}
}

func TestSummariesShow_CommandRendersBody(t *testing.T) {
	t.Parallel()
	s, sessID := seedSummariesFixtures(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	if err := runSummariesShow(t.Context(), c, sessID[:8], store.LLMKindSummary, false, &out); err != nil {
		t.Fatalf("runSummariesShow: %v", err)
	}
	if !strings.Contains(out.String(), "Refactor the ingest loop") {
		t.Errorf("show output missing topic:\n%s", out.String())
	}
}

func TestSummariesShow_JSONFlagEmitsRawBody(t *testing.T) {
	t.Parallel()
	s, sessID := seedSummariesFixtures(t)
	c := apiForStore(t, s)

	var out bytes.Buffer
	if err := runSummariesShow(t.Context(), c, sessID[:8], store.LLMKindSummary, true, &out); err != nil {
		t.Fatalf("runSummariesShow: %v", err)
	}
	var parsed prompts.SummaryResult
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("--json output not valid SummaryResult JSON: %v\n%s", err, out.String())
	}
	if parsed.Topic != "Refactor the ingest loop" {
		t.Errorf("topic round-trip: got %q", parsed.Topic)
	}
}

func TestSummariesShow_UnknownKindErrs(t *testing.T) {
	t.Parallel()
	s, sessID := seedSummariesFixtures(t)
	c := apiForStore(t, s)

	// This session only has a summary, not a reflection. Asking for
	// reflect on it should error with a clear "no output" message
	// rather than silently returning nothing.
	var out bytes.Buffer
	err := runSummariesShow(t.Context(), c, sessID[:8], store.LLMKindReflect, false, &out)
	if err == nil {
		t.Fatal("expected error when kind not present on session")
	}
	if !strings.Contains(err.Error(), "no reflect output") {
		t.Errorf("expected 'no reflect output' message, got %v", err)
	}
}

// seedSessionForMissing ingests one user_prompt event so a session
// row exists. Used to give the missing/fill paths something to
// surface. The summary slot is left empty; tests that want a
// summary on the session call SaveLLMOutput separately.
func seedSessionForMissing(t *testing.T, s *store.Store, sourceSession string, ts time.Time) string {
	t.Helper()
	env := events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts.UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     "first prompt for " + sourceSession,
		Payload:         map[string]any{},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, _ := s.DB().Begin()
	if _, _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed: %v", err)
	}
	_ = tx.Commit()
	return events.DeriveSessionID("claude-code", sourceSession)
}

func TestSummariesMissing_OnlyUnsummarizedRowsAppear(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Now()
	idA := seedSessionForMissing(t, s, "miss-A", now.Add(-time.Hour))
	idB := seedSessionForMissing(t, s, "miss-B", now.Add(-2*time.Hour))

	// Plant a summary on A only.
	tx, _ := s.DB().Begin()
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   ptrTo(idA),
		Kind:        store.LLMKindSummary,
		Model:       "test",
		PromptHash:  "h-A",
		Body:        `{"topic":"A"}`,
		CreatedAtMs: now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed summary: %v", err)
	}
	_ = tx.Commit()

	c := apiForStore(t, s)
	resp, err := c.SessionsMissingSummary(t.Context(), wire.SessionsMissingSummaryRequest{
		SinceMs: time.Now().Add(-24 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("SessionsMissingSummary: %v", err)
	}
	var out bytes.Buffer
	if err := writeMissingSummaries(&out, resp.Sessions, FormatTable); err != nil {
		t.Fatalf("writeMissingSummaries: %v", err)
	}
	body := out.String()
	if strings.Contains(body, idA[:8]) {
		t.Errorf("session A has a summary; should NOT appear:\n%s", body)
	}
	if !strings.Contains(body, idB[:8]) {
		t.Errorf("session B is missing a summary; should appear:\n%s", body)
	}
}

func TestSummariesMissing_JSONFormatShape(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := seedSessionForMissing(t, s, "miss-json", time.Now().Add(-time.Hour))

	c := apiForStore(t, s)
	resp, err := c.SessionsMissingSummary(t.Context(), wire.SessionsMissingSummaryRequest{
		SinceMs: time.Now().Add(-24 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("SessionsMissingSummary: %v", err)
	}
	var out bytes.Buffer
	if err := writeMissingSummaries(&out, resp.Sessions, FormatJSON); err != nil {
		t.Fatalf("writeMissingSummaries: %v", err)
	}

	var parsed []struct {
		ID          string `json:"id"`
		Cwd         string `json:"cwd"`
		FirstPrompt string `json:"first_prompt"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if len(parsed) != 1 || parsed[0].ID != id {
		t.Fatalf("expected one row with id=%s, got %+v", id, parsed)
	}
	if parsed[0].FirstPrompt != "first prompt for miss-json" {
		t.Errorf("unexpected first_prompt: %q", parsed[0].FirstPrompt)
	}
}

func TestSummariesMissing_EmptyWindowReportsCleanly(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	c := apiForStore(t, s)
	resp, err := c.SessionsMissingSummary(t.Context(), wire.SessionsMissingSummaryRequest{
		SinceMs: time.Now().Add(-24 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("SessionsMissingSummary: %v", err)
	}
	var out bytes.Buffer
	if err := writeMissingSummaries(&out, resp.Sessions, FormatTable); err != nil {
		t.Fatalf("writeMissingSummaries: %v", err)
	}
	if !strings.Contains(out.String(), "no sessions missing") {
		t.Errorf("empty result should print a friendly placeholder; got:\n%s", out.String())
	}
}

func TestRunSummariesFill_StreamsAndTallies(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := time.Now()
	idA := seedSessionForMissing(t, s, "fill-A", now.Add(-time.Hour))
	idB := seedSessionForMissing(t, s, "fill-B", now.Add(-2*time.Hour))

	rows := []wire.SessionDigest{
		{ID: idA},
		{ID: idB},
	}

	// fakeLLM returns a structured summary for any tool call. Both
	// sessions should "summarize" successfully and the tally line
	// should report 2 / 0 / 0.
	f := &fakeLLM{reply: ""}
	newClient := func() (llm.Client, error) { return f, nil }

	var out bytes.Buffer
	if err := runSummariesFill(t.Context(), s, apiForStore(t, s), newClient,
		rows, "", 5*time.Second, FormatTable, &out); err != nil {
		t.Fatalf("runSummariesFill: %v", err)
	}

	body := out.String()
	for _, want := range []string{
		"summarized",
		idA[:8],
		idB[:8],
		"filled: 2",
		// Per-row [i/N] prefix so the user sees progress instead
		// of a silent terminal during long batches. "[1/2]" must
		// appear on the first line (starting AND result), "[2/2]"
		// on the second.
		"[1/2]",
		"[2/2]",
		// "starting..." line must precede each LLM call so even a
		// 10s round-trip doesn't look like the command is frozen.
		"starting...",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	// Both sessions should now have a summary row.
	for _, id := range []string{idA, idB} {
		var n int
		_ = s.DB().QueryRow(
			`SELECT COUNT(*) FROM llm_outputs WHERE session_id=? AND kind='summary'`, id,
		).Scan(&n)
		if n != 1 {
			t.Errorf("session %s: expected 1 summary row after fill, got %d", id, n)
		}
	}
}

func TestRunSummariesFill_JSONFormatShape(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := seedSessionForMissing(t, s, "fill-json", time.Now().Add(-time.Hour))

	f := &fakeLLM{reply: ""}
	newClient := func() (llm.Client, error) { return f, nil }

	var out bytes.Buffer
	if err := runSummariesFill(t.Context(), s, apiForStore(t, s), newClient,
		[]wire.SessionDigest{{ID: id}}, "", 5*time.Second, FormatJSON, &out); err != nil {
		t.Fatalf("runSummariesFill: %v", err)
	}
	var parsed []fillStatus
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, out.String())
	}
	if len(parsed) != 1 {
		t.Fatalf("expected 1 result, got %d", len(parsed))
	}
	if parsed[0].SessionID != id {
		t.Errorf("session_id: got %q, want %q", parsed[0].SessionID, id)
	}
	if parsed[0].Status != "summarized" {
		t.Errorf("status: got %q, want summarized", parsed[0].Status)
	}
}
