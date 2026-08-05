package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
)

func TestRunFactsForSession_RequiresSummary(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{reply: "should not be called"}
	_, err := RunFactsForSession(context.Background(), apiForStore(t, s),
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
				Quote:        "go.mod requires Go 1.26",
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
	id, err := RunFactsForSession(context.Background(), apiForStore(t, s),
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

	facts, err := store.LoadFactsForSubject(t.Context(), s.DB(), "/work/systemd", 0, 0)
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
		if fact.EvidenceSessionID == nil || *fact.EvidenceSessionID != sessID {
			t.Errorf("evidence_session_id: got %v want %s", fact.EvidenceSessionID, sessID)
		}
	}

	// Render covers persisted count + each fact triple.
	got := out.String()
	for _, want := range []string{
		"2 fact(s) (2 persisted to semantic_facts)",
		"/work/systemd uses_language_version = Go 1.26",
		"/work/systemd runs_tests_via = go test ./...",
		"quote: go.mod requires Go 1.26",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestRunFactsForSession_DropsFactsKeyedToWrongCwd pins the
// anti-fabrication anchor: the facts prompt's hard rule says
// "Subject is the session's CWD", so a Subject pointing somewhere
// else is a hallucination. The seeded session's cwd is
// /work/systemd; the LLM emits one fact keyed to that and one to
// a fabricated /other/project — only the matching one survives.
func TestRunFactsForSession_DropsFactsKeyedToWrongCwd(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID,
		"go module audit",
		"go.mod requires Go 1.26; tests run via go test ./...")

	emit := prompts.FactsResult{
		Found: true,
		Facts: []prompts.InducedFact{
			{
				Subject:      "/work/systemd",
				Predicate:    "uses_language_version",
				Object:       "Go 1.26",
				Confidence:   0.95,
				Quote:        "go.mod requires Go 1.26",
				WhatHappened: "the user inspected go.mod",
			},
			{
				Subject:   "/other/project", // fabricated — not the session's cwd
				Predicate: "runs_tests_via",
				Object:    "pytest",
				// Quote also doesn't appear in the summary, but the
				// cwd filter would drop the row before the quote
				// check fires anyway.
				Confidence:   0.9,
				Quote:        "tests run via go test ./...",
				WhatHappened: "the user ran the test suite",
			},
		},
		Rationale: "extracted contracts",
	}
	toolInput, _ := json.Marshal(emit)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	id, err := RunFactsForSession(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunFactsForSession: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero llm_outputs row id")
	}

	// Only the cwd-anchored fact lands in semantic_facts.
	systemd, err := store.LoadFactsForSubject(t.Context(), s.DB(), "/work/systemd", 0, 0)
	if err != nil {
		t.Fatalf("LoadFactsForSubject: %v", err)
	}
	if len(systemd) != 1 {
		t.Errorf("expected 1 fact for /work/systemd, got %d", len(systemd))
	}
	other, err := store.LoadFactsForSubject(t.Context(), s.DB(), "/other/project", 0, 0)
	if err != nil {
		t.Fatalf("LoadFactsForSubject /other: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("expected zero facts for fabricated /other/project, got %d", len(other))
	}
}

// TestRunFactsForSession_DropsParaphrasedQuote pins the substrate
// substring check: a fact whose Quote isn't actually present in
// the session's summary + first_prompt is a paraphrase (the
// prompt's hard rule #2 forbids paraphrase). The schema lets a
// non-empty string through; this Go check is the post-decode
// substrate gate.
func TestRunFactsForSession_DropsParaphrasedQuote(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "go modules", "the project pins go 1.26 in go.mod")

	emit := prompts.FactsResult{
		Found: true,
		Facts: []prompts.InducedFact{
			{
				Subject:      "/work/systemd",
				Predicate:    "uses_language_version",
				Object:       "Go 1.26",
				Confidence:   0.95,
				Quote:        "the project pins Go 1.26 in go.mod", // exact (case-insensitive) substring
				WhatHappened: "good",
			},
			{
				Subject:      "/work/systemd",
				Predicate:    "uses_dependency",
				Object:       "modernc.org/sqlite",
				Confidence:   0.9,
				Quote:        "depends on modernc.org/sqlite via go.mod", // never in the summary — paraphrase
				WhatHappened: "bad",
			},
		},
		Rationale: "mix of grounded and paraphrased",
	}
	toolInput, _ := json.Marshal(emit)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	if _, err := RunFactsForSession(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID}, &out); err != nil {
		t.Fatalf("RunFactsForSession: %v", err)
	}

	facts, err := store.LoadFactsForSubject(t.Context(), s.DB(), "/work/systemd", 0, 0)
	if err != nil {
		t.Fatalf("LoadFactsForSubject: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact (grounded), got %d", len(facts))
	}
	if facts[0].Predicate != "uses_language_version" {
		t.Errorf("kept the wrong fact: %s", facts[0].Predicate)
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
	id, err := RunFactsForSession(context.Background(), apiForStore(t, s),
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
	if _, err := RunFactsForSession(context.Background(), apiForStore(t, s), newClient,
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
	if _, err := RunFactsForSession(context.Background(), apiForStore(t, s), newClient,
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
	if _, err := RunFactsForSession(context.Background(), apiForStore(t, s),
		func() (llm.Client, error) { return f, nil },
		FactsRunOptions{SessionID: sessID, JSON: true}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"found": false`) || !strings.Contains(got, `"rationale": "nope"`) {
		t.Errorf("expected raw JSON body, got:\n%s", got)
	}
}

// TestFactsSubstrate_CoversEverythingThePromptShows guards the
// grounding filter against discarding true facts.
//
// The substrate was summary + first_prompt only, but renderDigests
// also emits "Links observed" and "Shell commands observed
// (extracted from tool_use events)". A fact grounded in a command the
// prompt explicitly showed the model therefore failed the substring
// check and was dropped with a message that reads like an
// infrastructure error.
//
// The bias was systematic, not incidental: the documented predicate
// vocabulary leads with runs_tests_via / runs_build_via /
// runs_lint_via, all of which are naturally quoted from the shell
// commands rather than from summary prose.
func TestFactsSubstrate_CoversEverythingThePromptShows(t *testing.T) {
	t.Parallel()
	digest := prompts.SessionDigest{
		Cwd:           "/tmp/proj",
		Summary:       "worked on the build",
		FirstPrompt:   "fix the tests",
		ShellCommands: []string{"go test ./...", "golangci-lint run"},
		Links:         []string{"https://example.test/issues/7"},
	}
	substrate := factsGroundingSubstrate(digest)

	cases := []struct {
		name  string
		quote string
		want  bool
	}{
		{"quote from summary", "worked on the build", true},
		{"quote from first prompt", "fix the tests", true},
		{"quote from a shell command", "go test ./...", true},
		{"quote from another shell command", "golangci-lint run", true},
		{"quote from an observed link", "https://example.test/issues/7", true},
		{"case-insensitive match", "GO TEST ./...", true},
		{"quote the model invented", "make deploy-prod", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := factGrounded("/tmp/proj", digest.Cwd, tc.quote, substrate)
			if got != tc.want {
				t.Errorf("factGrounded(%q) = %v, want %v", tc.quote, got, tc.want)
			}
		})
	}
}
