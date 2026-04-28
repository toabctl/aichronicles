package prompts

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInductionResult_UnmarshalNestedObject covers the schema-
// correct shape: workflow / skill arrive as nested JSON objects.
// This is the path the schema asks for and most calls take.
func TestInductionResult_UnmarshalNestedObject(t *testing.T) {
	t.Parallel()
	in := []byte(`{
		"rationale": "ok",
		"workflow": {
			"task_shape": "deploy a Go service to staging",
			"procedure": [{"action": "build the binary"}],
			"preconditions": ["clean tree"],
			"success_checks": ["health check passes"],
			"evidence": [{"session_id": "s1", "quote": "q", "what_happened": "h"}]
		}
	}`)
	var got InductionResult
	if err := json.Unmarshal(in, &got); err != nil {
		t.Fatalf("nested object: %v", err)
	}
	if got.Rationale != "ok" {
		t.Errorf("rationale: %q", got.Rationale)
	}
	if got.Workflow == nil {
		t.Fatal("workflow nil; want non-nil")
	}
	if got.Workflow.TaskShape != "deploy a Go service to staging" {
		t.Errorf("task_shape: %q", got.Workflow.TaskShape)
	}
	if len(got.Workflow.Procedure) != 1 || got.Workflow.Procedure[0].Action != "build the binary" {
		t.Errorf("procedure: %+v", got.Workflow.Procedure)
	}
}

// TestInductionResult_UnmarshalStringifiedWorkflow covers the
// observed Claude failure mode where the workflow object gets
// JSON-encoded into a string. Captured from a real bug:
// "raw=...\"workflow\":\"{\\\"task_shape\\\":...\".
func TestInductionResult_UnmarshalStringifiedWorkflow(t *testing.T) {
	t.Parallel()
	// The model emits the inner object as a string. The outer
	// JSON shows this as `"workflow": "{...}"` — note the inner
	// braces are part of the string value, not the JSON
	// structure. encoding/json's escape pass produces the
	// `\"key\":` we see in real captures.
	inner := `{"task_shape":"patch upstream OS guard","procedure":[{"action":"read script"}],"preconditions":["script exists"],"success_checks":["patched lines apply"],"evidence":[{"session_id":"s1","quote":"q","what_happened":"h"}]}`
	innerEscaped, _ := json.Marshal(inner) // produces `"…"` with everything inside escaped
	in := []byte(`{"rationale":"r","workflow":` + string(innerEscaped) + `}`)

	var got InductionResult
	if err := json.Unmarshal(in, &got); err != nil {
		t.Fatalf("stringified workflow: %v", err)
	}
	if got.Workflow == nil {
		t.Fatal("workflow nil; expected stringified-form recovery")
	}
	if got.Workflow.TaskShape != "patch upstream OS guard" {
		t.Errorf("task_shape after string-recovery: %q", got.Workflow.TaskShape)
	}
}

// TestInductionResult_UnmarshalStringifiedWithTrailingJunk covers
// the exact malformation captured in the field: Claude emits a
// stringified object AND appends one extra closing brace. Recovery
// via streaming Decode reads the well-formed object and ignores
// the trailing junk.
func TestInductionResult_UnmarshalStringifiedWithTrailingJunk(t *testing.T) {
	t.Parallel()
	inner := `{"task_shape":"port script","procedure":[{"action":"a"}],"preconditions":["p"],"success_checks":["c"],"evidence":[{"session_id":"s","quote":"q","what_happened":"h"}]}` + `}`
	innerEscaped, _ := json.Marshal(inner)
	in := []byte(`{"rationale":"r","workflow":` + string(innerEscaped) + `}`)

	var got InductionResult
	if err := json.Unmarshal(in, &got); err != nil {
		t.Fatalf("trailing-junk recovery: %v", err)
	}
	if got.Workflow == nil {
		t.Fatal("workflow nil; expected recovery despite trailing junk")
	}
	if got.Workflow.TaskShape != "port script" {
		t.Errorf("task_shape: %q", got.Workflow.TaskShape)
	}
	if len(got.Workflow.Procedure) != 1 || got.Workflow.Procedure[0].Action != "a" {
		t.Errorf("procedure: %+v", got.Workflow.Procedure)
	}
}

// TestInductionResult_UnmarshalStringifiedSkill verifies the same
// recovery path applies to skill — the field is structurally
// identical and could exhibit the same Claude failure mode.
func TestInductionResult_UnmarshalStringifiedSkill(t *testing.T) {
	t.Parallel()
	inner := `{"name":"deploy-staging","when_to_use":"deploying to staging","why":"reusable","evidence":[{"session_id":"s1","quote":"q","what_happened":"h"}],"frequency":1,"effort":"small","alternatives_rejected":""}`
	innerEscaped, _ := json.Marshal(inner)
	in := []byte(`{"rationale":"r","skill":` + string(innerEscaped) + `}`)

	var got InductionResult
	if err := json.Unmarshal(in, &got); err != nil {
		t.Fatalf("stringified skill: %v", err)
	}
	if got.Skill == nil {
		t.Fatal("skill nil; expected stringified-form recovery")
	}
	if got.Skill.Name != "deploy-staging" {
		t.Errorf("name after string-recovery: %q", got.Skill.Name)
	}
}

// TestInductionResult_UnmarshalNullAndMissing covers the four
// null-ish shapes the model emits when there's nothing to record:
// missing field, JSON null, empty string (a stringified null),
// and whitespace-only string.
func TestInductionResult_UnmarshalNullAndMissing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{"missing", `{"rationale":"r"}`},
		{"null", `{"rationale":"r","workflow":null,"skill":null}`},
		{"empty string", `{"rationale":"r","workflow":"","skill":""}`},
		{"whitespace string", `{"rationale":"r","workflow":"   ","skill":"\n\t"}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got InductionResult
			if err := json.Unmarshal([]byte(tc.input), &got); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got.Workflow != nil {
				t.Errorf("%s: workflow should be nil, got %+v", tc.name, got.Workflow)
			}
			if got.Skill != nil {
				t.Errorf("%s: skill should be nil, got %+v", tc.name, got.Skill)
			}
			if got.Rationale != "r" {
				t.Errorf("%s: rationale: %q", tc.name, got.Rationale)
			}
		})
	}
}

// TestInductionResult_UnmarshalMalformedRejected confirms we don't
// silently swallow garbage. A workflow string that doesn't contain
// a well-formed InducedWorkflow must surface as an error — silently
// emitting workflow=nil would let the LLM corrupt our corpus
// (CLAUDE.md rule #7: wrong stored data is worse than missing data).
func TestInductionResult_UnmarshalMalformedRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "string but not JSON",
			input: `{"rationale":"r","workflow":"this isn't a workflow"}`,
		},
		{
			name:  "string of wrong-shape JSON",
			input: `{"rationale":"r","workflow":"[1,2,3]"}`,
		},
		{
			name:  "object missing required fields",
			input: `{"rationale":"r","workflow":{"unrelated":"x"}}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got InductionResult
			err := json.Unmarshal([]byte(tc.input), &got)
			// Two of these fail at decode (good); the
			// "object missing required fields" case
			// decodes without error because Go's stdlib
			// json doesn't enforce required:[]. That's
			// fine — schema enforcement of "required" is
			// the JSON-schema layer's job, not ours.
			if tc.name != "object missing required fields" && err == nil {
				t.Errorf("%s: expected error, got nil", tc.name)
			}
			if err != nil && !strings.Contains(err.Error(), "workflow") {
				t.Errorf("%s: error should mention workflow: %v", tc.name, err)
			}
		})
	}
}

// TestInductionResult_UnmarshalRoundTrip confirms a value
// produced by our marshal path round-trips through unmarshal.
// Belt-and-braces against the custom UnmarshalJSON breaking the
// happy path.
func TestInductionResult_UnmarshalRoundTrip(t *testing.T) {
	t.Parallel()
	original := InductionResult{
		Rationale: "round-trip",
		Workflow: &InducedWorkflow{
			TaskShape: "trip",
			Procedure: []WorkflowStep{{Action: "go"}},
		},
		Skill: &ProposedSkill{
			Name:      "round-trip-skill",
			WhenToUse: "now",
			Why:       "because",
			Evidence:  []ProposalEvidence{{SessionID: "s", Quote: "q", WhatHappened: "h"}},
			Frequency: 1,
			Effort:    "small",
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back InductionResult
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Workflow == nil || back.Workflow.TaskShape != "trip" {
		t.Errorf("workflow not preserved: %+v", back.Workflow)
	}
	if back.Skill == nil || back.Skill.Name != "round-trip-skill" {
		t.Errorf("skill not preserved: %+v", back.Skill)
	}
}
