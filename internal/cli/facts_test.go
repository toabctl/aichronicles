package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

func TestRunFactsForSession_RequiresSummary(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{reply: "should not be called"}
	_, err := RunFactsForSession(context.Background(), s, apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no summary") {
		t.Fatalf("expected 'no summary' error, got %v", err)
	}
	if f.called != 0 {
		t.Errorf("LLM should not be called when summary is missing, got %d calls", f.called)
	}
}

func TestRunFactsForSession_PersistsFactsIntoSemanticFacts(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID,
		"go module audit",
		"go.mod requires Go 1.26; tests run via go test ./...; deploys via systemd timer")

	emit := prompts.FactsResult{
		Found: true,
		Facts: []prompts.InducedFact{
			{
				Subject:      "/work/systemd",
				Predicate:    "uses_language_version",
				Object:       "Go 1.26",
				Confidence:   0.95,
				Quote:        "go.mod requires 1.26",
				WhatHappened: "the user inspected go.mod",
			},
			{
				Subject:      "/work/systemd",
				Predicate:    "runs_tests_via",
				Object:       "go test ./...",
				Confidence:   0.9,
				Quote:        "tests run via go test ./...",
				WhatHappened: "the user ran the test suite",
			},
		},
		Rationale: "extracted go-mod and test contract from session",
	}
	toolInput, _ := json.Marshal(emit)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	id, err := RunFactsForSession(context.Background(), s, apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunFactsForSession: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero llm_outputs row id")
	}

	// The two-layer persistence invariant: llm_outputs has the row,
	// semantic_facts has both individual facts.
	row, err := store.LoadLLMOutputByID(t.Context(), s.DB(), id)
	if err != nil {
		t.Fatalf("LoadLLMOutputByID: %v", err)
	}
	if row == nil || row.Kind != store.LLMKindFacts {
		t.Fatalf("expected llm_outputs row of kind=facts, got %+v", row)
	}

	facts, err := store.LoadFactsForSubject(t.Context(), s.DB(), "/work/systemd", 0)
	if err != nil {
		t.Fatalf("LoadFactsForSubject: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 semantic_facts rows, got %d", len(facts))
	}
	// Both facts must point back to the LLM_output that birthed them.
	for _, fact := range facts {
		if fact.SourceLLMOutputID != id {
			t.Errorf("fact %s/%s/%s source_llm_output_id=%d, want %d",
				fact.Subject, fact.Predicate, fact.Object, fact.SourceLLMOutputID, id)
		}
		if !fact.EvidenceSessionID.Valid || fact.EvidenceSessionID.String != sessID {
			t.Errorf("evidence_session_id: got %v want %s", fact.EvidenceSessionID, sessID)
		}
	}

	// Render covers persisted count + each fact triple.
	got := out.String()
	for _, want := range []string{
		"2 fact(s) (2 persisted to semantic_facts)",
		"/work/systemd uses_language_version = Go 1.26",
		"/work/systemd runs_tests_via = go test ./...",
		"quote: go.mod requires 1.26",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q\n--- output ---\n%s", want, got)
		}
	}
}

func TestRunFactsForSession_NoFactsFoundDoesNotPersist(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "Q&A about generics", "discussed Go type parameter semantics")

	emit := prompts.FactsResult{
		Found:     false,
		Facts:     []prompts.InducedFact{},
		Rationale: "session was a generics Q&A; no project-level facts asserted",
	}
	toolInput, _ := json.Marshal(emit)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	id, err := RunFactsForSession(context.Background(), s, apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunFactsForSession: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero llm_outputs row id even on no-facts verdict")
	}

	// Zero rows in semantic_facts.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM semantic_facts`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("no-facts verdict should not write semantic_facts rows, got %d", n)
	}

	got := out.String()
	if !strings.Contains(got, "no facts") {
		t.Errorf("expected 'no facts' in render:\n%s", got)
	}
}

func TestRunFactsForSession_CacheHitSkipsLLMAndStillRendersFacts(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "deploy", "ran the staging deploy")

	emit := prompts.FactsResult{
		Found: true,
		Facts: []prompts.InducedFact{
			{
				Subject:      "/work/systemd",
				Predicate:    "deploys_to",
				Object:       "staging via systemd timer",
				Confidence:   0.85,
				Quote:        "ran the staging deploy",
				WhatHappened: "deploy",
			},
		},
		Rationale: "extracted deploy target",
	}
	toolInput, _ := json.Marshal(emit)
	f := &fakeLLM{toolInput: toolInput}
	newClient := func() (llm.Client, error) { return f, nil }

	// First run populates the cache + persists facts.
	if _, err := RunFactsForSession(context.Background(), s, apiForStore(t, s), newClient,
		FactsRunOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if f.called != 1 {
		t.Fatalf("first: LLM call count: got %d want 1", f.called)
	}

	// Second run hits the cache (no extra LLM call) but the
	// facts must still be rendered + the upsert refreshes
	// asserted_at_ms via SaveSemanticFact's ON CONFLICT DO UPDATE.
	var out2 bytes.Buffer
	if _, err := RunFactsForSession(context.Background(), s, apiForStore(t, s), newClient,
		FactsRunOptions{SessionID: sessID}, &out2); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.called != 1 {
		t.Errorf("cache miss on second run: calls=%d, want 1", f.called)
	}
	if !strings.Contains(out2.String(), "deploys_to = staging via systemd timer") {
		t.Errorf("cached run did not re-render facts:\n%s", out2.String())
	}

	// Single semantic_facts row by PK invariant (idempotent re-run).
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM semantic_facts`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 fact (idempotent on re-run), got %d", n)
	}
}

func TestRunFactsForSession_JSONFormatEmitsRawBody(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "x", "y")

	emit := prompts.FactsResult{Found: false, Rationale: "nope"}
	toolInput, _ := json.Marshal(emit)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	if _, err := RunFactsForSession(context.Background(), s, apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID, JSON: true}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"found": false`) || !strings.Contains(got, `"rationale": "nope"`) {
		t.Errorf("expected raw JSON body, got:\n%s", got)
	}
}
