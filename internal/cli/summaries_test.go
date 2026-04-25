package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// seedSummariesFixtures writes one of each kind so the list/show
// commands have something to render. Returns the store and the full
// session id used for the `summary` row.
func seedSummariesFixtures(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := testStore(t)

	// One ingest-shaped event so the session exists for the summary
	// row's foreign key.
	env := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sum-fixture",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"x": 1},
		Redaction:       &ingest.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, _ := s.DB().Begin()
	if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed event: %v", err)
	}
	_ = tx.Commit()
	sessID := ingest.DeriveSessionID("claude-code", "sum-fixture")

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
			{Name: "extract-metrics", WhenToUse: "w", Why: "y", SessionIDs: []string{"s"}},
		},
	})

	now := time.Now().UnixMilli()
	tx, _ = s.DB().Begin()
	defer func() { _ = tx.Commit() }()

	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: sessID, Valid: true},
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
	if err := writeSummariesTable(&out, rows); err != nil {
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
	if err := writeSummariesTable(&out, rows); err != nil {
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
			{Name: "first", WhenToUse: "w", Why: "y", SessionIDs: []string{"s"}},
			{Name: "second", WhenToUse: "w", Why: "y", SessionIDs: []string{"s"}},
			{Name: "third", WhenToUse: "w", Why: "y", SessionIDs: []string{"s"}},
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

	// Drive the actual cobra command to catch arg/flag wiring.
	cmd := newSummariesShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--db", dbPathFromStore(t, s), sessID[:8]})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "Refactor the ingest loop") {
		t.Errorf("show output missing topic:\n%s", out.String())
	}
}

func TestSummariesShow_JSONFlagEmitsRawBody(t *testing.T) {
	t.Parallel()
	s, sessID := seedSummariesFixtures(t)

	cmd := newSummariesShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--db", dbPathFromStore(t, s), "--json", sessID[:8]})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
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

	// This session only has a summary, not a reflection. Asking for
	// reflect on it should error with a clear "no output" message
	// rather than silently returning nothing.
	cmd := newSummariesShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--db", dbPathFromStore(t, s), "--kind", "reflect", sessID[:8]})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when kind not present on session")
	}
	if !strings.Contains(err.Error(), "no reflect output") {
		t.Errorf("expected 'no reflect output' message, got %v", err)
	}
}

// dbPathFromStore extracts the backing file path from a Store opened
// on disk. Tests use an in-memory test store with a known path so
// subcommands (which open stores by path) can target the same DB.
func dbPathFromStore(t *testing.T, s *store.Store) string {
	t.Helper()
	var path string
	err := s.DB().QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &path)
	if err != nil {
		t.Fatalf("pragma database_list: %v", err)
	}
	return path
}
