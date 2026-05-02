package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// seedSessionInCwd creates one session at the given cwd with a
// user_prompt event and an optional summary. Returns the session id.
func seedSessionInCwd(t *testing.T, st *store.Store, cwd, prompt, topic string, ts time.Time) string {
	t.Helper()
	sourceID := "src-" + cwd + "-" + uuid.NewString()
	env := events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceID,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts,
		Cwd:             cwd,
		ContentText:     prompt,
		Payload:         map[string]any{},
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sessID := events.DeriveSessionID("claude-code", sourceID)

	if topic != "" {
		body, _ := json.Marshal(prompts.SummaryResult{
			Topic:        topic,
			WhatWasDone:  []string{"x"},
			Unresolved:   []string{},
			KeyFiles:     []string{},
			Links:        []prompts.LinkAnnotation{},
			Subagents:    []prompts.SubagentSummary{},
			SessionLinks: []prompts.SessionLinkAnnotation{},
		})
		if _, err := st.DB().Exec(
			`INSERT INTO llm_outputs(session_id, kind, body, prompt_hash, model, created_at_ms)
			 VALUES (?, 'summary', ?, ?, 'fake-model', ?)`,
			sessID, string(body), "h-"+sessID, ts.UnixMilli(),
		); err != nil {
			t.Fatalf("seed summary: %v", err)
		}
	}
	return sessID
}

func TestGetProjectContext_RequiresCwd(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_project_context", `{"cwd":"  "}`)
	if !strings.Contains(res.Content[0].Text, "cwd is required") {
		t.Errorf("expected validation error, got %+v", res)
	}
}

func TestGetProjectContext_EmptyProjectShowsAllSectionsWithEmptyStateMessages(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t) // uses cwds /work/sess-foo, /work/sess-bar
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(s, st)

	res := callTool(t, s, "get_project_context", `{"cwd":"/no-such-project"}`)
	body := res.Content[0].Text

	// All five section headers must appear even when sections are
	// empty — that's the contract that lets the agent know "this
	// is the full picture; nothing was truncated."
	for _, want := range []string{
		"# Project context: /no-such-project",
		"## Recent sessions in this cwd",
		"## Open unresolved threads",
		"## Project facts",
		"## Recent workflows",
		"## Skills installed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing section header %q\n--- body ---\n%s", want, body)
		}
	}
	for _, want := range []string{
		"first session in this cwd",
		"wrapped up cleanly",
		"facts induce --session",
		"induction sweep", // workflow + skill share this hint after the Round 8 merge
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing empty-state hint %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestGetProjectContext_PopulatedProjectRendersEverySection(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	const cwd = "/work/myproj"

	now := time.Now().UTC()
	// Two sessions in the cwd — both with summaries so the topic
	// renders.
	a := seedSessionInCwd(t, st, cwd, "investigate the deploy",
		"investigated the staging deploy pipeline", now.Add(-2*time.Hour))
	b := seedSessionInCwd(t, st, cwd, "fix the build",
		"build broke; rolled back the dep", now.Add(-1*time.Hour))
	_ = a
	_ = b

	// Add an unresolved item via a fresh summary on session b.
	body, _ := json.Marshal(prompts.SummaryResult{
		Topic:        "build broke; rolled back the dep",
		WhatWasDone:  []string{"rolled back"},
		Unresolved:   []string{"add a regression test for the rollback path"},
		KeyFiles:     []string{},
		Links:        []prompts.LinkAnnotation{},
		Subagents:    []prompts.SubagentSummary{},
		SessionLinks: []prompts.SessionLinkAnnotation{},
	})
	if _, err := st.DB().Exec(
		`UPDATE llm_outputs SET body = ? WHERE session_id = ? AND kind = 'summary'`,
		string(body), b,
	); err != nil {
		t.Fatalf("update summary: %v", err)
	}

	// Seed a semantic fact for the cwd.
	loID := seedFactsRow(t, st)
	if _, err := store.SaveSemanticFact(t.Context(), st.DB(), store.SemanticFact{
		SourceLLMOutputID: loID,
		Subject:           cwd,
		Predicate:         "uses_language_version",
		Object:            "Go 1.26",
		Confidence:        0.95,
		AssertedAtMs:      time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("save fact: %v", err)
	}

	// Seed a found workflow (project-agnostic — surfaced regardless
	// of cwd). After Round 8 workflows live inside kind=induction
	// rows in body.workflow, not in their own kind.
	wfBody, _ := json.Marshal(map[string]any{
		"workflow": map[string]any{
			"task_shape": "deploy a backend service to staging",
			"procedure": []map[string]any{
				// Each {placeholder} is declared on its step so the
				// unmarshal-time AWM consistency check accepts the fixture.
				{
					"action": "Tag the release commit with {version}",
					"placeholders": []map[string]any{
						{"token": "version", "description": "release version", "example": "v1.2.3"},
					},
				},
				{
					"action": "Run kubectl rollout for {service-name}",
					"placeholders": []map[string]any{
						{"token": "service-name", "description": "k8s service", "example": "api"},
					},
				},
			},
			"preconditions":  []string{},
			"success_checks": []string{},
			"evidence":       []any{},
		},
		"rationale": "extracted abstract deploy procedure",
	})
	if _, err := st.DB().Exec(
		`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
		 VALUES ('induction', 'fake-model', ?, ?, ?)`,
		"h-ind-"+t.Name(), string(wfBody), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed induction-with-workflow: %v", err)
	}

	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(srv, st)

	res := callTool(t, srv, "get_project_context", `{"cwd":"/work/myproj","since_days":30}`)
	out := res.Content[0].Text

	// Recent sessions: both summary topics must render.
	for _, want := range []string{
		"investigated the staging deploy pipeline",
		"build broke; rolled back the dep",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("recent-sessions section missing %q\n--- body ---\n%s", want, out)
		}
	}
	// Unresolved item.
	if !strings.Contains(out, "add a regression test for the rollback path") {
		t.Errorf("unresolved item missing\n--- body ---\n%s", out)
	}
	// Fact.
	if !strings.Contains(out, "uses_language_version = Go 1.26") {
		t.Errorf("fact missing\n--- body ---\n%s", out)
	}
	// Workflow.
	if !strings.Contains(out, "deploy a backend service to staging") {
		t.Errorf("workflow missing\n--- body ---\n%s", out)
	}
	if !strings.Contains(out, "Tag the release commit with {version} →") {
		t.Errorf("workflow procedure preview missing\n--- body ---\n%s", out)
	}
}

func TestGetProjectContext_RespectsMaxPerSection(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	const cwd = "/work/manysessions"

	now := time.Now().UTC()
	for i := range 8 {
		seedSessionInCwd(t, st, cwd,
			"session "+string(rune('a'+i)),
			"topic-"+string(rune('a'+i)),
			now.Add(-time.Duration(i)*time.Minute))
	}

	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(srv, st)

	// max_per_section=3 should cap recent sessions at 3.
	res := callTool(t, srv, "get_project_context", `{"cwd":"/work/manysessions","max_per_section":3}`)
	out := res.Content[0].Text

	// Count the number of session-line bullets in the recent-
	// sessions section. We pick a unique substring that's only on
	// session bullets ("topic-") and count occurrences.
	gotSessions := strings.Count(out, "topic-")
	if gotSessions != 3 {
		t.Errorf("max_per_section=3 should cap session lines: got %d, want 3\n--- body ---\n%s",
			gotSessions, out)
	}
}

func TestGetProjectContext_FiltersWorkflowsToFoundOnly(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)

	// Two induction rows: one carries a workflow inline, one
	// emits no workflow (Round 8: workflows live inside kind=induction
	// rows in body.workflow). Only the with-workflow row should
	// render in the workflows section; the no-workflow row's
	// rationale must NOT leak.
	{
		body, _ := json.Marshal(map[string]any{
			"workflow": map[string]any{
				"task_shape":     "ship a feature",
				"procedure":      []any{map[string]any{"action": "do it"}},
				"preconditions":  []string{},
				"success_checks": []string{},
				"evidence":       []any{},
			},
			"rationale": "extracted",
		})
		if _, err := st.DB().Exec(
			`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
			 VALUES ('induction', 'fake-model', ?, ?, ?)`,
			"h-ind-yes-"+t.Name(), string(body), time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("seed yes: %v", err)
		}
	}
	{
		body, _ := json.Marshal(map[string]any{
			"rationale": "session was a one-off",
		})
		if _, err := st.DB().Exec(
			`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
			 VALUES ('induction', 'fake-model', ?, ?, ?)`,
			"h-ind-no-"+t.Name(), string(body), time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("seed no: %v", err)
		}
	}

	srv := New(ServerInfo{Name: "ac", Version: "0.1"}, nil)
	RegisterAichroniclesTools(srv, st)
	res := callTool(t, srv, "get_project_context", `{"cwd":"/some/cwd"}`)
	out := res.Content[0].Text
	if !strings.Contains(out, "ship a feature") {
		t.Errorf("workflow-bearing induction row missing:\n%s", out)
	}
	if strings.Contains(out, "session was a one-off") {
		t.Errorf("no-workflow induction row's rationale leaked into context:\n%s", out)
	}
}
