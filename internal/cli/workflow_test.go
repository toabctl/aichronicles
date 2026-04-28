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

func TestRunWorkflowForSession_RequiresSummary(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	f := &fakeLLM{reply: "should not be called"}
	_, err := RunWorkflowForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		WorkflowRunOptions{SessionID: sessID}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no summary") {
		t.Fatalf("expected 'no summary' error, got %v", err)
	}
	if f.called != 0 {
		t.Errorf("LLM should not be called when summary is missing, got %d calls", f.called)
	}
}

func TestRunWorkflowForSession_PersistsFoundWorkflow(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID,
		"deploy-staging walkthrough",
		"ran the staging deploy across two services and patched the config drift")

	wf := prompts.WorkflowResult{
		Found:     true,
		TaskShape: "deploy a backend service to staging",
		Procedure: []prompts.WorkflowStep{
			{Action: "Tag the release commit with {version}",
				Placeholders: []prompts.WorkflowPlaceholder{
					{Token: "version", Description: "semver", Example: "v0.42.0"},
				}},
			{Action: "Run kubectl rollout for {service-name}"},
		},
		Preconditions: []string{"git working tree is clean"},
		SuccessChecks: []string{"kubectl rollout status returns rollout complete"},
		Evidence: []prompts.WorkflowEvidence{{
			SessionID:    sessID,
			Quote:        "ran the staging deploy across two services and patched the config drift",
			WhatHappened: "the user followed the staging deploy recipe",
		}},
		Rationale: "extracted the kubectl-rollout pattern observed twice",
	}
	toolInput, _ := json.Marshal(wf)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	id, err := RunWorkflowForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		WorkflowRunOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunWorkflowForSession: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero row id")
	}

	// Verify persistence + kind.
	row, err := store.LoadLLMOutputByID(t.Context(), s.DB(), id)
	if err != nil {
		t.Fatalf("LoadLLMOutputByID: %v", err)
	}
	if row == nil {
		t.Fatal("expected a workflow row")
	}
	if row.Kind != store.LLMKindWorkflow {
		t.Errorf("kind: got %q want %q", row.Kind, store.LLMKindWorkflow)
	}
	if !row.SessionID.Valid || row.SessionID.String != sessID {
		t.Errorf("session_id: got %v want %s", row.SessionID, sessID)
	}

	// Render covers the procedure + placeholders.
	outStr := out.String()
	for _, want := range []string{
		`"deploy a backend service to staging"`,
		"Tag the release commit with {version}",
		"{version} — semver",
		"e.g. v0.42.0",
		"kubectl rollout for {service-name}",
		"git working tree is clean",
	} {
		if !strings.Contains(outStr, want) {
			t.Errorf("render missing %q\n--- output ---\n%s", want, outStr)
		}
	}
}

func TestRunWorkflowForSession_PersistsNoWorkflowFound(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "one-off bug fix", "fixed a typo in config.yaml")

	wf := prompts.WorkflowResult{
		Found:     false,
		Rationale: "session was a one-off bug fix; no recurring procedure",
	}
	toolInput, _ := json.Marshal(wf)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	id, err := RunWorkflowForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		WorkflowRunOptions{SessionID: sessID}, &out)
	if err != nil {
		t.Fatalf("RunWorkflowForSession: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero row id even on no-workflow verdict — the verdict is the value")
	}
	outStr := out.String()
	if !strings.Contains(outStr, "no workflow") {
		t.Errorf("expected 'no workflow' in render:\n%s", outStr)
	}
	if !strings.Contains(outStr, "one-off bug fix") {
		t.Errorf("rationale missing from render:\n%s", outStr)
	}
}

func TestRunWorkflowForSession_CacheHitSkipsLLM(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "deploy", "ran the staging deploy")

	wf := prompts.WorkflowResult{
		Found:     true,
		TaskShape: "deploy a service",
		Procedure: []prompts.WorkflowStep{{Action: "Tag and rollout"}},
		Evidence: []prompts.WorkflowEvidence{{
			SessionID:    sessID,
			Quote:        "ran the staging deploy",
			WhatHappened: "deploy",
		}},
		Rationale: "extracted",
	}
	toolInput, _ := json.Marshal(wf)
	f := &fakeLLM{toolInput: toolInput}
	newClient := func() (llm.Client, error) { return f, nil }

	// First run populates the cache.
	if _, err := RunWorkflowForSession(context.Background(), s, newClient,
		WorkflowRunOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if f.called != 1 {
		t.Fatalf("first: LLM call count: got %d, want 1", f.called)
	}

	// Second run with same session must hit cache.
	if _, err := RunWorkflowForSession(context.Background(), s, newClient,
		WorkflowRunOptions{SessionID: sessID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.called != 1 {
		t.Errorf("second run should cache-hit: LLM called %d times total, want 1", f.called)
	}

	// --force bypasses the cache.
	if _, err := RunWorkflowForSession(context.Background(), s, newClient,
		WorkflowRunOptions{SessionID: sessID, Force: true}, &bytes.Buffer{}); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if f.called != 2 {
		t.Errorf("--force should re-call LLM: calls=%d, want 2", f.called)
	}
}

func TestRunWorkflowForSession_JSONFormatEmitsRawBody(t *testing.T) {
	t.Parallel()
	s, sessID := seedSessionForSummarize(t)
	plantSummary(t, s, sessID, "deploy", "ran the staging deploy")
	wf := prompts.WorkflowResult{
		Found: false, Rationale: "nope",
	}
	toolInput, _ := json.Marshal(wf)
	f := &fakeLLM{toolInput: toolInput}

	var out bytes.Buffer
	if _, err := RunWorkflowForSession(context.Background(), s,
		func() (llm.Client, error) { return f, nil },
		WorkflowRunOptions{SessionID: sessID, JSON: true}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"found": false`) || !strings.Contains(got, `"rationale": "nope"`) {
		t.Errorf("expected raw JSON body, got:\n%s", got)
	}
}
