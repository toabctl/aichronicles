package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

func TestWorkflowsPage_Empty(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/workflows")
	if status != http.StatusOK {
		t.Fatalf("status: %d; body=%s", status, body)
	}
	for _, want := range []string{
		"Workflows",
		"No workflows induced yet",
		"induction sweep",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestWorkflowsPage_RendersFoundRows(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	const sessID = "00000000-0000-0000-0000-000000000abc"
	if _, err := st.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES (?, 'claude-code', 'src-x')`,
		sessID,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"workflow": map[string]any{
			"task_shape": "deploy a backend service to staging",
			"procedure": []map[string]any{
				// `{version}` is declared in this step's placeholders;
				// without that, the unmarshal-time AWM consistency
				// check would (correctly) reject the fixture.
				{
					"action": "Tag the release commit with {version}",
					"placeholders": []map[string]any{
						{"token": "version", "description": "release version", "example": "v1.2.3"},
					},
				},
				{"action": "Run kubectl rollout"},
			},
			"preconditions":  []string{"git working tree clean"},
			"success_checks": []string{"kubectl rollout status returns complete"},
			"evidence":       []any{},
		},
		"rationale": "extracted",
	})
	if _, err := st.DB().Exec(
		`INSERT INTO llm_outputs(session_id, kind, model, prompt_hash, body, created_at_ms)
		 VALUES (?, 'induction', 'fake-model', ?, ?, ?)`,
		sessID, "h-wf-"+t.Name(), string(body), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed induction: %v", err)
	}

	_, page := fetch(t, base+"/workflows")
	for _, want := range []string{
		"deploy a backend service to staging",
		"Tag the release commit with {version}",
		"Run kubectl rollout",
		"git working tree clean",
		"kubectl rollout status returns complete",
		`href="/sessions/` + sessID,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, page)
		}
	}
}

func TestWorkflowsPage_OmitsInductionRowsWithoutWorkflow(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	// Seed an induction row that only has a skill, no workflow —
	// must NOT render on /workflows.
	noWorkflowBody, _ := json.Marshal(map[string]any{
		"skill": map[string]any{
			"name":                  "skill-only",
			"when_to_use":           "x",
			"why":                   "y",
			"evidence":              []any{map[string]any{"session_id": "s", "quote": "q", "what_happened": "w"}},
			"frequency":             1,
			"effort":                "small",
			"alternatives_rejected": "none",
		},
		"rationale": "skill yes, workflow no",
	})
	if _, err := st.DB().Exec(
		`INSERT INTO llm_outputs(kind, model, prompt_hash, body, created_at_ms)
		 VALUES ('induction', 'fake-model', ?, ?, ?)`,
		"h-no-wf-"+t.Name(), string(noWorkflowBody), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, body := fetch(t, base+"/workflows")
	if strings.Contains(body, "skill-only") {
		t.Errorf("induction row without workflow leaked into /workflows:\n%s", body)
	}
	if !strings.Contains(body, "No workflows induced yet") {
		t.Errorf("expected empty-state message:\n%s", body)
	}
	_ = store.LLMKindInduction
}
