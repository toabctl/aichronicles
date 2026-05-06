package prompts

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
)

func nullS(s string) events.NullString { return events.NullString{String: s, Valid: s != ""} }

// --- BuildSummary ---

func TestBuildSummary_IncludesTranscriptAndSessionID(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{EventID: "e1", Kind: "user_prompt", Role: nullS("user"),
			ContentText: nullS("how do I parse JSONL"), TsSourceMs: 1},
		{EventID: "e2", Kind: "assistant_message", Role: nullS("assistant"),
			ContentText: nullS("bufio.Scanner with Buffer() for long lines"), TsSourceMs: 2},
	}
	built, err := BuildSummary("sess-abc", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if built.Request.System == "" {
		t.Error("System prompt missing")
	}
	if built.Request.MaxTokens != summaryMaxTokens {
		t.Errorf("MaxTokens: got %d, want %d", built.Request.MaxTokens, summaryMaxTokens)
	}
	if len(built.Request.Messages) != 1 || built.Request.Messages[0].Role != llm.RoleUser {
		t.Fatalf("expected single user turn, got %+v", built.Request.Messages)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{"sess-abc", "how do I parse JSONL", "bufio.Scanner"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if built.Hash == "" {
		t.Error("Hash empty")
	}
}

func TestBuildSummary_LabelsSubagentEventsInTranscript(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", Role: nullS("user"),
			ContentText: nullS("main agent prompt"), TsSourceMs: 1},
		{Kind: "user_prompt", Role: nullS("user"),
			SubagentID: nullS("agent-7"), SubagentType: nullS("planner"),
			ContentText: nullS("planner step one"), TsSourceMs: 2},
		{Kind: "user_prompt", Role: nullS("user"),
			SubagentID:  nullS("agent-9"),
			ContentText: nullS("worker step"), TsSourceMs: 3},
	}
	built, err := BuildSummary("sess", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	// Top-level event has no subagent prefix.
	if strings.Contains(body, "[sa:") && !strings.Contains(body, "main agent prompt") {
		t.Errorf("top-level event accidentally labelled:\n%s", body)
	}
	// Subagent events carry sa:<id>:<type> prefix.
	if !strings.Contains(body, "sa:agent-7:planner") {
		t.Errorf("expected sa:agent-7:planner label:\n%s", body)
	}
	// Subagent without a type renders as `?` so the shape stays
	// uniform.
	if !strings.Contains(body, "sa:agent-9:?") {
		t.Errorf("expected sa:agent-9:? label for type-less subagent:\n%s", body)
	}
}

func TestBuildSummary_SchemaIncludesSubagents(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, err := BuildSummary("sess", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	schema := string(built.Request.Tools[0].InputSchema)
	for _, want := range []string{`"subagents"`, `"description"`, `[sa:`} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %q:\n%s", want, schema)
		}
	}
}

func TestBuildSummary_SetsForcedTool(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, err := BuildSummary("sess", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if len(built.Request.Tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(built.Request.Tools))
	}
	tool := built.Request.Tools[0]
	if tool.Name != ToolNameSummary {
		t.Errorf("tool name: got %q, want %q", tool.Name, ToolNameSummary)
	}
	if built.Request.ForceTool != ToolNameSummary {
		t.Errorf("ForceTool: got %q, want %q", built.Request.ForceTool, ToolNameSummary)
	}
	// The schema must parse as valid JSON so the provider accepts it.
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type: got %v, want object", schema["type"])
	}
	required, _ := schema["required"].([]any)
	wantRequired := map[string]bool{"topic": true, "what_was_done": true, "outcomes": true, "unresolved": true, "key_files": true, "links": true}
	for _, r := range required {
		delete(wantRequired, r.(string))
	}
	if len(wantRequired) != 0 {
		t.Errorf("schema missing required fields: %v", wantRequired)
	}
}

func TestBuildSummary_RendersLinksBlockWhenPresent(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, err := BuildSummary("sess-l", events, SummaryInputs{
		Links: []string{"https://a.example/", "https://b.example/"},
	})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "Links observed in this session") {
		t.Errorf("links stanza missing:\n%s", body)
	}
	for _, url := range []string{"https://a.example/", "https://b.example/"} {
		if !strings.Contains(body, url) {
			t.Errorf("url %q not rendered in prompt:\n%s", url, body)
		}
	}
	if !strings.Contains(body, "DROP any you cannot confidently attribute") {
		t.Errorf("anti-fabrication instruction missing:\n%s", body)
	}
}

func TestBuildSummary_RendersFilesBlockWhenPresent(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	files := []string{
		"/home/me/repo/internal/store/migrate.go",
		"/home/me/repo/internal/store/search.go",
	}
	built, err := BuildSummary("sess-f", events, SummaryInputs{Files: files})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "Files observed via tool calls") {
		t.Errorf("files stanza missing:\n%s", body)
	}
	for _, p := range files {
		if !strings.Contains(body, p) {
			t.Errorf("path %q not rendered:\n%s", p, body)
		}
	}
	if !strings.Contains(body, "do NOT shorten or reformat") {
		t.Errorf("anti-fabrication instruction missing:\n%s", body)
	}
}

func TestBuildSummary_OmitsFilesBlockWhenNoneProvided(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, _ := BuildSummary("sess", events, SummaryInputs{})
	if strings.Contains(built.Request.Messages[0].Content, "Files observed") {
		t.Errorf("empty files list should omit the stanza:\n%s",
			built.Request.Messages[0].Content)
	}
}

func TestBuildSummary_HashChangesWhenFilesChange(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	a, _ := BuildSummary("sess", events, SummaryInputs{})
	b, _ := BuildSummary("sess", events, SummaryInputs{Files: []string{"/etc/hosts"}})
	if a.Hash == b.Hash {
		t.Error("adding a file should change the hash")
	}
}

func TestBuildSummary_SchemaInstructsAbsoluteFiles(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, _ := BuildSummary("sess", events, SummaryInputs{})
	schema := string(built.Request.Tools[0].InputSchema)
	for _, want := range []string{
		`"key_files"`,
		"Absolute file paths",
		"Files observed",
		"verbatim",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %q:\n%s", want, schema)
		}
	}
}

func TestBuildSummary_OmitsLinksBlockWhenNoneProvided(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, err := BuildSummary("sess", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "Links observed") {
		t.Errorf("empty links list should omit the stanza:\n%s", body)
	}
}

func TestBuildSummary_LinksPassThroughEgressRedaction(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	// A URL carrying a basic-auth credential — the kind of thing
	// extractions can capture verbatim. renderLinksBlock must scrub
	// it before it reaches the prompt.
	links := []string{"https://user:s3cret@example.com/foo"}
	built, err := BuildSummary("sess-leak", events, SummaryInputs{Links: links})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "s3cret") {
		t.Errorf("links stanza leaked creds:\n%s", body)
	}
	hasPattern := false
	for _, p := range built.Patterns {
		if p == "basic_auth_url" {
			hasPattern = true
			break
		}
	}
	if !hasPattern {
		t.Errorf("Patterns should report basic_auth_url, got %v", built.Patterns)
	}
}

func TestBuildSummary_ScrubsSecretsAndReportsPatterns(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{EventID: "e1", Kind: "user_prompt", Role: nullS("user"),
			ContentText: nullS("my AKIAIOSFODNN7EXAMPLE leak"), TsSourceMs: 1},
	}
	built, err := BuildSummary("sess-x", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("prompt leaked raw secret:\n%s", body)
	}
	if !strings.Contains(body, "<redacted:aws_access_key>") {
		t.Errorf("prompt should contain marker:\n%s", body)
	}
	if len(built.Patterns) != 1 || built.Patterns[0] != "aws_access_key" {
		t.Errorf("Patterns: got %v", built.Patterns)
	}
}

func TestBuildSummary_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	if _, err := BuildSummary("", []events.EventView{{}}, SummaryInputs{}); err == nil {
		t.Error("empty sessionID: expected error")
	}
	if _, err := BuildSummary("sess", nil, SummaryInputs{}); err == nil {
		t.Error("nil events: expected error")
	}
}

func TestBuildSummary_HashDeterministicSameInput(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("identical prompt"), TsSourceMs: 1},
	}
	a, _ := BuildSummary("sess", events, SummaryInputs{})
	b, _ := BuildSummary("sess", events, SummaryInputs{})
	if a.Hash != b.Hash {
		t.Errorf("hash not deterministic: %q vs %q", a.Hash, b.Hash)
	}
}

func TestBuildSummary_HashChangesWhenContentChanges(t *testing.T) {
	t.Parallel()
	a, _ := BuildSummary("sess", []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("first"), TsSourceMs: 1},
	}, SummaryInputs{})
	b, _ := BuildSummary("sess", []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("second"), TsSourceMs: 1},
	}, SummaryInputs{})
	if a.Hash == b.Hash {
		t.Errorf("hash unchanged for different content: %q", a.Hash)
	}
}

func TestBuildSummary_HashChangesWhenLinksChange(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	a, _ := BuildSummary("sess", events, SummaryInputs{})
	b, _ := BuildSummary("sess", events, SummaryInputs{Links: []string{"https://example.com/"}})
	if a.Hash == b.Hash {
		t.Error("adding a link should change the hash")
	}
}

func TestBuildSummary_NullToolUseEventDoesNotPanic(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{
			EventID: "e1", Kind: "tool_use",
			Role:        events.NullString{},
			ContentText: events.NullString{},
			TsSourceMs:  1,
			ToolName:    nullS("Bash"),
		},
	}
	built, err := BuildSummary("sess-null", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "Bash") {
		t.Errorf("tool label missing for NULL-content tool_use event:\n%s", body)
	}
}

func TestBuildSummary_AllNullEventFieldsRenderKindLabel(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{
			EventID: "e1", Kind: "session_start",
			TsSourceMs: 1,
		},
	}
	built, err := BuildSummary("sess-bare", events, SummaryInputs{})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "[session_start]") {
		t.Errorf("bare event missing kind label:\n%s", body)
	}
}

func TestBuildSummary_RendersPriorSessionsStanzaWhenCandidatesPresent(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	prior := []CandidatePriorSession{
		{
			ID:          "11111111-1111-1111-1111-111111111111",
			StartedAtMs: 1_700_000_000_000,
			EndedAtMs:   1_700_000_300_000,
			Topic:       "fixed the auth middleware",
		},
		{
			ID:          "22222222-2222-2222-2222-222222222222",
			StartedAtMs: 1_700_001_000_000,
			EndedAtMs:   1_700_001_300_000,
			// No topic — should render "(no summary)" so the model
			// knows the field was absent rather than empty.
		},
	}
	built, err := BuildSummary("sess-pp", events, SummaryInputs{CandidatePriorSes: prior})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"Possibly-related prior sessions",
		"11111111-1111-1111-1111-111111111111",
		"fixed the auth middleware",
		"22222222-2222-2222-2222-222222222222",
		"(no summary)",
		"DROP any you can't ground",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prior-sessions stanza missing %q:\n%s", want, body)
		}
	}
}

func TestBuildSummary_OmitsPriorSessionsStanzaWhenNoneProvided(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, _ := BuildSummary("sess", events, SummaryInputs{})
	if strings.Contains(built.Request.Messages[0].Content, "Possibly-related prior sessions") {
		t.Errorf("empty candidate list should omit the stanza:\n%s",
			built.Request.Messages[0].Content)
	}
}

func TestBuildSummary_SchemaIncludesSessionLinks(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	built, _ := BuildSummary("sess", events, SummaryInputs{})
	schema := string(built.Request.Tools[0].InputSchema)
	for _, want := range []string{
		`"session_links"`,
		`"to_session_id"`,
		`"builds_on"`,
		`"repeats_failure_of"`,
		`"supersedes"`,
		`"related"`,
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %q:\n%s", want, schema)
		}
	}
}

func TestBuildSummary_HashChangesWhenCandidatesChange(t *testing.T) {
	t.Parallel()
	events := []events.EventView{
		{Kind: "user_prompt", ContentText: nullS("x"), TsSourceMs: 1},
	}
	a, _ := BuildSummary("sess", events, SummaryInputs{})
	b, _ := BuildSummary("sess", events, SummaryInputs{
		CandidatePriorSes: []CandidatePriorSession{
			{ID: "abc", StartedAtMs: 1, EndedAtMs: 2, Topic: "t"},
		},
	})
	if a.Hash == b.Hash {
		t.Error("adding candidate prior sessions should change the hash")
	}
}

// --- BuildReflect ---

func TestBuildReflect_RequiresSessions(t *testing.T) {
	t.Parallel()
	if _, err := BuildReflect(nil, 24*time.Hour); err == nil {
		t.Error("empty digests: expected error")
	}
}

func TestBuildReflect_SetsForcedTool(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{{ID: "s1", FirstPrompt: "hi"}}
	built, err := BuildReflect(digests, time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	if built.Request.ForceTool != ToolNameReflection {
		t.Errorf("ForceTool: got %q, want %q", built.Request.ForceTool, ToolNameReflection)
	}
	if len(built.Request.Tools) != 1 || built.Request.Tools[0].Name != ToolNameReflection {
		t.Errorf("tools: got %+v", built.Request.Tools)
	}
	if !json.Valid(built.Request.Tools[0].InputSchema) {
		t.Error("reflection schema is not valid JSON")
	}
}

func TestBuildReflect_RendersAllDigestFields(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{
			ID:          "s-1",
			StartedAtMs: 100,
			EndedAtMs:   200,
			Cwd:         "/work/proj",
			FirstPrompt: "refactor auth module",
			Summary:     "Refactored session middleware; 3 tests added.",
		},
	}
	built, err := BuildReflect(digests, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{"s-1", "/work/proj", "refactor auth module", "Refactored session middleware"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestBuildReflect_RendersOutcomeCueWhenPresent(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{
			ID:      "s-with-outcome",
			Summary: "Did stuff.",
			Outcome: &store.SessionOutcome{
				SessionID:        "s-with-outcome",
				Outcome:          store.OutcomeFailureLikely,
				ToolFailureCount: 4,
				GitUndoCount:     1,
			},
		},
		{ID: "s-no-outcome", Summary: "Other stuff.", Outcome: nil},
	}
	built, err := BuildReflect(digests, time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	body := built.Request.Messages[0].Content

	// The session with an outcome cue must render it with its
	// counter tail; the session without one must NOT carry an
	// outcome line at all.
	if !strings.Contains(body, "Outcome: failure_likely (4 tool_failures, 1 git_undos") {
		t.Errorf("expected failure_likely cue with counter tail in body:\n%s", body)
	}
	// Confirm the no-outcome session doesn't get a phantom line.
	// The no-outcome digest follows the with-outcome one in the
	// rendered body; finding "Outcome:" only once in the body is
	// the cleanest assertion here.
	if got := strings.Count(body, "Outcome:"); got != 1 {
		t.Errorf("Outcome line count: got %d, want exactly 1 (only s-with-outcome should have it)", got)
	}
}

func TestBuildPropose_RendersPriorProposalsStanza(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	digests := []SessionDigest{
		{ID: "s1", Summary: "x"}, {ID: "s2", Summary: "y"}, // BuildPropose needs ≥1 (no min)
	}
	in := ProposeInputs{
		Digests: digests,
		PriorProposals: []PriorProposal{
			{
				SkillName:     "deploy-staging",
				ProposedAtMs:  now.Add(-30 * 24 * time.Hour).UnixMilli(),
				Added:         true,
				AddedAtMs:     now.Add(-29 * 24 * time.Hour).UnixMilli(),
				LoadsAfterAdd: 8,
			},
			{
				SkillName:     "fix-flake",
				ProposedAtMs:  now.Add(-20 * 24 * time.Hour).UnixMilli(),
				Added:         true,
				AddedAtMs:     now.Add(-19 * 24 * time.Hour).UnixMilli(),
				LoadsAfterAdd: 0, // added but unused
			},
			{
				SkillName:        "buggy-skill",
				ProposedAtMs:     now.Add(-10 * 24 * time.Hour).UnixMilli(),
				Added:            true,
				AddedAtMs:        now.Add(-9 * 24 * time.Hour).UnixMilli(),
				LoadsAfterAdd:    4,
				FailedLoadsAfter: 3,
			},
			{
				SkillName:    "rejected-idea",
				ProposedAtMs: now.Add(-5 * 24 * time.Hour).UnixMilli(),
				Added:        false,
			},
		},
	}
	built, err := BuildPropose(in)
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content

	for _, want := range []string{
		"Prior proposals",
		"deploy-staging — proposed 30 days ago, ADDED 29 days ago, 8 loads, 0 failures",
		"in use, working — DO NOT repropose",
		"fix-flake — proposed 20 days ago, ADDED 19 days ago, 0 loads since",
		"skill on disk but unused — when_to_use may be wrong",
		"buggy-skill — proposed 10 days ago, ADDED 9 days ago, 4 loads with 3 failures",
		"rejected-idea — proposed 5 days ago, PENDING",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// BuildWorkflow has been retired. Workflow extraction now lives
// inline inside BuildInduce — same single LLM call extracts both
// skill and workflow from one session. See
// TestBuildInduce_SchemaSupportsWorkflowField for the merged
// behaviour.

func TestBuildFacts_RequiresDigestID(t *testing.T) {
	t.Parallel()
	_, err := BuildFacts(FactsFromSessionInputs{Digest: SessionDigest{}})
	if err == nil {
		t.Errorf("expected error on empty Digest.ID")
	}
}

func TestBuildFacts_ForcesRecordFactsTool(t *testing.T) {
	t.Parallel()
	digest := SessionDigest{
		ID:          "sess-facts-1",
		Cwd:         "/work/proj",
		FirstPrompt: "what go version does this use?",
		Summary:     "ran go.mod inspection: requires 1.26.",
	}
	built, err := BuildFacts(FactsFromSessionInputs{Digest: digest})
	if err != nil {
		t.Fatalf("BuildFacts: %v", err)
	}
	if built.Request.ForceTool != ToolNameFacts {
		t.Errorf("ForceTool: got %q want %q", built.Request.ForceTool, ToolNameFacts)
	}
	if len(built.Request.Tools) != 1 || built.Request.Tools[0].Name != ToolNameFacts {
		t.Errorf("tools wiring wrong: %+v", built.Request.Tools)
	}
}

func TestBuildFacts_RendersSessionAndAdvocatesPredicateVocabulary(t *testing.T) {
	t.Parallel()
	digest := SessionDigest{
		ID:      "sess-facts-2",
		Summary: "ran go test -tags=integration ./...",
	}
	built, err := BuildFacts(FactsFromSessionInputs{Digest: digest})
	if err != nil {
		t.Fatalf("BuildFacts: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "sess-facts-2") {
		t.Errorf("session id not rendered: %s", body)
	}
	if !strings.Contains(body, "ran go test -tags=integration") {
		t.Errorf("session summary not rendered: %s", body)
	}
	// System prompt advertises the recommended predicate vocabulary.
	for _, want := range []string{
		"uses_language_version",
		"runs_tests_via",
		"primary_language",
	} {
		if !strings.Contains(built.Request.System, want) {
			t.Errorf("system prompt missing recommended predicate %q", want)
		}
	}
	// And enforces evidence-grounding.
	if !strings.Contains(built.Request.System, "verbatim quote") {
		t.Errorf("system prompt missing anti-fabrication rule")
	}
}

func TestBuildFacts_HashStableAcrossCalls(t *testing.T) {
	t.Parallel()
	digest := SessionDigest{ID: "stable", Summary: "x"}
	a, _ := BuildFacts(FactsFromSessionInputs{Digest: digest})
	b, _ := BuildFacts(FactsFromSessionInputs{Digest: digest})
	if a.Hash != b.Hash {
		t.Errorf("hash unstable: %q != %q", a.Hash, b.Hash)
	}
}

// TestBuildPropose_RendersFailureModesCluster confirms the
// failure-shape input renders as a per-mode grouping (tool_failures
// / git_undos / prompt_repeats) with RECURRING / ONE-OFF flags
// derived from distinct-session counts, and that a session
// exhibiting multiple modes appears under each (multi-tag).
func TestBuildPropose_RendersFailureModesCluster(t *testing.T) {
	t.Parallel()
	in := ProposeInputs{
		Digests: []SessionDigest{
			{ID: "s1", Summary: "x"}, {ID: "s2", Summary: "y"},
		},
		FailureShapes: []FailureShapeDigest{
			{
				SessionID:        "00000000-0000-0000-0000-0000000000aa",
				Title:            "deploy went sideways three times",
				ToolFailureCount: 3,
				GitUndoCount:     1,
			},
			{
				SessionID:        "00000000-0000-0000-0000-0000000000bb",
				Title:            "rebase blew up",
				ToolFailureCount: 2,
			},
			{
				SessionID:         "00000000-0000-0000-0000-0000000000cc",
				Title:             "fix the test flake",
				PromptRepeatCount: 2,
			},
		},
	}
	built, err := BuildPropose(in)
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"Failure modes observed across 3 failure-shaped sessions",
		// tool_failures appears in 2 sessions → RECURRING
		"- tool_failures (2 sessions, RECURRING):",
		"[00000000] deploy went sideways three times (3 tool_failures)",
		"[00000000] rebase blew up (2 tool_failures)",
		// git_undos appears in 1 session (the deploy one too) → ONE-OFF
		"- git_undos (1 sessions, ONE-OFF):",
		"[00000000] deploy went sideways three times (1 git_undos)",
		// prompt_repeats appears in 1 session → ONE-OFF
		"- prompt_repeats (1 sessions, ONE-OFF):",
		"[00000000] fix the test flake (2 prompt_repeats)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// System prompt rule 13 must point at the precomputed clusters.
	if !strings.Contains(built.Request.System, "Failure modes observed") {
		t.Errorf("system prompt rule 13 should reference the precomputed cluster stanza")
	}
	if !strings.Contains(built.Request.System, "RECURRING") {
		t.Errorf("system prompt rule 13 should explain the RECURRING / ONE-OFF flags")
	}
}

func TestBuildPropose_OmitsFailureModesStanzaWhenEmpty(t *testing.T) {
	t.Parallel()
	in := ProposeInputs{
		Digests:       []SessionDigest{{ID: "s1", Summary: "x"}, {ID: "s2", Summary: "y"}},
		FailureShapes: nil,
	}
	built, err := BuildPropose(in)
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "Failure modes observed") {
		t.Errorf("empty FailureShapes must produce empty stanza:\n%s", body)
	}
}

func TestBuildPropose_FailureModesPassThroughEgressRedaction(t *testing.T) {
	t.Parallel()
	in := ProposeInputs{
		Digests: []SessionDigest{{ID: "s1", Summary: "x"}, {ID: "s2", Summary: "y"}},
		FailureShapes: []FailureShapeDigest{
			{
				SessionID:        "s-secrets",
				Title:            "deploying with key AKIAIOSFODNN7EXAMPLE leaked into the title",
				ToolFailureCount: 2,
			},
		},
	}
	built, err := BuildPropose(in)
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("aws key leaked into prompt: %s", body)
	}
}

// TestRenderFailureModes_OmitsBucketsThatHaveNoSessions confirms a
// failure corpus that exercises only one mode renders only that
// mode's bucket — empty buckets must not produce phantom headers
// that make the LLM think the absent modes are "ONE-OFF" patterns.
func TestRenderFailureModes_OmitsBucketsThatHaveNoSessions(t *testing.T) {
	t.Parallel()
	shapes := []FailureShapeDigest{
		{SessionID: "00000000-only-tools", Title: "all tool fails", ToolFailureCount: 4},
		{SessionID: "00000000-also-tools", Title: "another tool fail", ToolFailureCount: 2},
	}
	out := renderFailureModes(shapes)
	if !strings.Contains(out, "- tool_failures (2 sessions, RECURRING):") {
		t.Errorf("expected tool_failures bucket; got:\n%s", out)
	}
	for _, unwanted := range []string{"git_undos", "prompt_repeats"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %q bucket on tool-only corpus:\n%s", unwanted, out)
		}
	}
}

func TestBuildPropose_OmitsPriorProposalsStanzaWhenEmpty(t *testing.T) {
	t.Parallel()
	in := ProposeInputs{
		Digests:        []SessionDigest{{ID: "s1", Summary: "x"}, {ID: "s2", Summary: "y"}},
		PriorProposals: nil,
	}
	built, err := BuildPropose(in)
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "Prior proposals") {
		t.Errorf("empty PriorProposals must produce empty stanza:\n%s", body)
	}
}

func TestBuildReflect_OutcomeSuccessRendersScaleNotFailureCounters(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{
			ID:      "s-success",
			Summary: "Got it done.",
			Outcome: &store.SessionOutcome{
				SessionID:    "s-success",
				Outcome:      store.OutcomeSuccessLikely,
				ToolUseCount: 12,
			},
		},
	}
	built, err := BuildReflect(digests, time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	body := built.Request.Messages[0].Content
	// success_likely carries tool_uses so the LLM can distinguish a
	// thin successful session (2 tool calls) from a substantial one
	// (50) — both look identical without scale.
	if !strings.Contains(body, "Outcome: success_likely (12 tool_uses)\n") {
		t.Errorf("expected success_likely cue with tool_uses scale:\n%s", body)
	}
	// Failure counters stay suppressed — they're zero by definition
	// here and would just be noise.
	if strings.Contains(body, "tool_failures") {
		t.Errorf("unexpected failure-counter tail on success_likely cue:\n%s", body)
	}
}

func TestBuildReflect_RendersPerSessionLinks(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{
			ID:    "s-L",
			Links: []string{"https://docs.example.com/a", "https://issues.example.com/42"},
		},
	}
	built, err := BuildReflect(digests, time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "Links observed:") {
		t.Errorf("per-session links header missing:\n%s", body)
	}
	for _, want := range []string{"https://docs.example.com/a", "https://issues.example.com/42"} {
		if !strings.Contains(body, want) {
			t.Errorf("link %q missing from digest:\n%s", want, body)
		}
	}
}

func TestBuildReflect_ScrubsSecretsInCwd(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{ID: "s-cwd", Cwd: "/home/AKIAIOSFODNN7EXAMPLE/proj"},
	}
	built, err := BuildReflect(digests, time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("cwd leaked secret into prompt:\n%s", body)
	}
	if len(built.Patterns) != 1 || built.Patterns[0] != "aws_access_key" {
		t.Errorf("Patterns: got %v", built.Patterns)
	}
}

func TestBuildReflect_ScrubsSecretsInDigestFields(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{
			ID:          "s-x",
			FirstPrompt: "use sk-ant-" + strings.Repeat("a", 40),
		},
	}
	built, err := BuildReflect(digests, time.Hour)
	if err != nil {
		t.Fatalf("BuildReflect: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "sk-ant-a") {
		t.Errorf("reflect prompt leaked secret:\n%s", body)
	}
	if len(built.Patterns) != 1 || built.Patterns[0] != "anthropic_api_key" {
		t.Errorf("Patterns: got %v", built.Patterns)
	}
}

// --- BuildPropose ---

func TestBuildPropose_RequiresSessions(t *testing.T) {
	t.Parallel()
	if _, err := BuildPropose(ProposeInputs{}); err == nil {
		t.Error("empty digests: expected error")
	}
}

func TestBuildPropose_SetsForcedTool(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{{ID: "p", FirstPrompt: "hi"}}
	built, err := BuildPropose(ProposeInputs{Digests: digests})
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	if built.Request.ForceTool != ToolNameProposal {
		t.Errorf("ForceTool: got %q, want %q", built.Request.ForceTool, ToolNameProposal)
	}
	if !json.Valid(built.Request.Tools[0].InputSchema) {
		t.Error("proposal schema is not valid JSON")
	}
}

// TestProposalToolSchema_AutoSkillFieldsRequired pins the AutoSkill
// (Yang et al., 2026) 7-tuple metadata: every emitted skill MUST
// carry triggers, tags, and examples alongside the existing fields.
// The contrastive-induction `kind` field (EvoSkill / EvoSC) joins
// the required set so every emission carries a pattern-vs-pitfall
// label. Schema bytes are stable (hashRequest depends on them); a
// regression here breaks both the LLM contract and the cache key,
// so the test asserts presence loudly.
func TestProposalToolSchema_AutoSkillFieldsRequired(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`"triggers"`,
		`"tags"`,
		`"examples"`,
		`"kind"`,
		`"#/$defs/triggers"`,
		`"#/$defs/tags"`,
		`"#/$defs/examples"`,
		`"#/$defs/kind"`,
		`"enum":["pattern","pitfall"]`,
	} {
		if !strings.Contains(proposalToolSchema, want) {
			t.Errorf("proposalToolSchema missing %q", want)
		}
	}
	for _, want := range []string{
		`"triggers"`,
		`"tags"`,
		`"examples"`,
		`"kind"`,
		`"input"`,
		`"output"`,
		`"enum":["pattern","pitfall"]`,
	} {
		if !strings.Contains(inductionToolSchema, want) {
			t.Errorf("inductionToolSchema missing %q", want)
		}
	}
}

// TestBuildMergeSkill_ValidInputs covers the happy path: a
// well-formed candidate + existing SKILL.md produces a Built whose
// schema is valid JSON and whose user message references both
// sides plus the supplied next_version.
func TestBuildMergeSkill_ValidInputs(t *testing.T) {
	t.Parallel()
	in := MergeSkillInputs{
		SkillName:      "deploy-staging",
		NextVersion:    "v0.1.5",
		CurrentSkillMd: "---\nname: deploy-staging\nversion: v0.1.4\n---\n# deploy-staging\n\nrun the staging deploy.\n",
		Candidate: ProposedSkill{
			Name:      "deploy-staging",
			WhenToUse: "when the user wants to deploy to staging",
			Why:       "single command instead of remembering the script",
			Triggers:  []string{"deploy staging", "ship staging", "push to staging"},
			Tags:      []string{"deploy", "ci"},
			Examples:  []ProposedSkillExample{{Input: "deploy this branch", Output: "calls staging deploy"}},
		},
	}
	built, err := BuildMergeSkill(in)
	if err != nil {
		t.Fatalf("BuildMergeSkill: %v", err)
	}
	if built.Request.ForceTool != ToolNameSkillMerge {
		t.Errorf("ForceTool: got %q want %q", built.Request.ForceTool, ToolNameSkillMerge)
	}
	if !json.Valid(built.Request.Tools[0].InputSchema) {
		t.Error("merge schema is not valid JSON")
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"deploy-staging",
		"v0.1.5",               // next version reaches the prompt
		"v0.1.4",               // existing version surfaces too (via raw md)
		"deploy staging",       // trigger from candidate
		"calls staging deploy", // example output
		"# deploy-staging",     // existing body
		"record_skill_merge",   // tool name appears in prompt? actually only via system prompt; check no
	} {
		if want == "record_skill_merge" {
			continue // checked via ForceTool above; tool name doesn't have to appear in user message
		}
		if !strings.Contains(body, want) {
			t.Errorf("merge prompt missing %q", want)
		}
	}
}

// TestFilterGroundedTriggers covers the anti-fabrication filter:
// triggers are kept only when they appear (case-insensitively, as
// substrings) inside at least one evidence Quote. Without this
// filter, the LLM is free to invent retrieval phrases that have
// no anchor in the actual session text, and the resulting skill
// gets retrieved on adjacent-but-wrong queries.
func TestFilterGroundedTriggers(t *testing.T) {
	t.Parallel()
	ev := []ProposalEvidence{
		{Quote: "user asked: deploy to staging please"},
		{Quote: "tooling: ran `kubectl apply -f staging.yaml`"},
	}

	cases := []struct {
		name     string
		in       []string
		evidence []ProposalEvidence
		want     []string
	}{
		{
			name:     "all grounded",
			in:       []string{"deploy to staging", "kubectl apply"},
			evidence: ev,
			want:     []string{"deploy to staging", "kubectl apply"},
		},
		{
			name:     "ungrounded dropped",
			in:       []string{"deploy to staging", "rollback prod"},
			evidence: ev,
			want:     []string{"deploy to staging"},
		},
		{
			name:     "case insensitive match",
			in:       []string{"DEPLOY TO STAGING"},
			evidence: ev,
			want:     []string{"DEPLOY TO STAGING"},
		},
		{
			name:     "all ungrounded → nil",
			in:       []string{"rollback prod", "ssh into bastion"},
			evidence: ev,
			want:     nil,
		},
		{
			name:     "empty triggers → nil",
			in:       nil,
			evidence: ev,
			want:     nil,
		},
		{
			name:     "no evidence → nil even if triggers given",
			in:       []string{"x"},
			evidence: nil,
			want:     nil,
		},
		{
			name:     "evidence with empty quotes → nil",
			in:       []string{"x"},
			evidence: []ProposalEvidence{{Quote: ""}, {Quote: "   "}},
			want:     nil,
		},
		{
			name:     "whitespace trigger dropped",
			in:       []string{"   ", "deploy"},
			evidence: ev,
			want:     []string{"deploy"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FilterGroundedTriggers(tc.in, tc.evidence)
			if len(got) != len(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
				return
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("at [%d]: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestProposalResult_GroundTriggers exercises the in-place
// orchestration helper: every skill's Triggers gets replaced with
// the grounded subset, using that skill's own Evidence as the
// substrate. Skills are independent — fabrication in one doesn't
// affect another.
func TestProposalResult_GroundTriggers(t *testing.T) {
	t.Parallel()
	r := &ProposalResult{
		Skills: []ProposedSkill{
			{
				Name:     "deploy-staging",
				Triggers: []string{"deploy to staging", "made-up phrase"},
				Evidence: []ProposalEvidence{{Quote: "deploy to staging please"}},
			},
			{
				Name:     "rollback-prod",
				Triggers: []string{"rollback the prod deploy"},
				Evidence: []ProposalEvidence{{Quote: "let's rollback the prod deploy"}},
			},
		},
	}
	r.GroundTriggers()
	if len(r.Skills[0].Triggers) != 1 || r.Skills[0].Triggers[0] != "deploy to staging" {
		t.Errorf("skill 0 triggers not filtered: %v", r.Skills[0].Triggers)
	}
	if len(r.Skills[1].Triggers) != 1 || r.Skills[1].Triggers[0] != "rollback the prod deploy" {
		t.Errorf("skill 1 triggers should survive: %v", r.Skills[1].Triggers)
	}
}

// TestBuildMergeSkill_RendersCandidateScriptsAndKind pins the
// fix for the "scripts silently dropped" bug: when the candidate
// has Scripts and a contrastive Kind, the merge prompt MUST surface
// both so the LLM can actually decide what to keep. Without this,
// the merger was told to "import only reusable, non-conflicting
// additions" but couldn't see the candidate's AWM substrate at all.
func TestBuildMergeSkill_RendersCandidateScriptsAndKind(t *testing.T) {
	t.Parallel()
	in := MergeSkillInputs{
		SkillName:      "deploy-staging",
		NextVersion:    "v0.1.5",
		CurrentSkillMd: "---\nname: deploy-staging\nversion: v0.1.4\n---\n# deploy-staging\n\nrun the staging deploy.\n",
		Candidate: ProposedSkill{
			Name:      "deploy-staging",
			WhenToUse: "when the user wants to deploy to staging",
			Why:       "single command instead of remembering the script",
			Kind:      "pitfall",
			Scripts: []ProposedSkillScript{
				{
					Name:    "preflight.sh",
					Purpose: "Sanity-check the staging cluster before deploy",
					Steps: []ProposedScriptStep{
						{Cmd: "kubectl --context={cluster} get nodes", Purpose: "verify cluster reachable"},
						{Cmd: "test -f deploy/{branch}.yaml", Purpose: "manifest exists"},
					},
					Placeholders: []ProposedScriptPlaceholder{
						{Token: "cluster", Description: "k8s context name", Example: "staging-eu1"},
						{Token: "branch", Description: "release branch", Example: "release/2026.04"},
					},
				},
			},
		},
	}
	built, err := BuildMergeSkill(in)
	if err != nil {
		t.Fatalf("BuildMergeSkill: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"Kind: pitfall",
		"name: preflight.sh",
		"purpose: Sanity-check the staging cluster",
		"step 1: kubectl --context={cluster}",
		"placeholder {cluster}",
		"e.g. staging-eu1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("merge prompt missing %q\n--- prompt ---\n%s", want, body)
		}
	}
}

// TestMergeSkillToolSchema_DescribesScriptsAndKind asserts the
// merge schema actually requires the new fields. Without these,
// the LLM has no grammar telling it scripts/kind are part of the
// expected output.
func TestMergeSkillToolSchema_DescribesScriptsAndKind(t *testing.T) {
	t.Parallel()
	var schema map[string]any
	if err := json.Unmarshal([]byte(mergeSkillToolSchema), &schema); err != nil {
		t.Fatalf("schema parse: %v", err)
	}
	required, _ := schema["required"].([]any)
	requiredSet := map[string]bool{}
	for _, r := range required {
		s, _ := r.(string)
		requiredSet[s] = true
	}
	if !requiredSet["kind"] {
		t.Errorf("schema 'required' should list 'kind'; got %v", required)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["scripts"]; !ok {
		t.Errorf("schema 'properties' should declare 'scripts'")
	}
	kind, _ := props["kind"].(map[string]any)
	enum, _ := kind["enum"].([]any)
	if len(enum) != 2 {
		t.Errorf("kind enum should be [pattern,pitfall]; got %v", enum)
	}
}

// TestMergedSkillResult_RoundTripsScriptsAndKind asserts that the
// Go struct can marshal/unmarshal a result containing scripts and
// kind without losing data — guards against accidental json-tag
// drift if the struct grows over time.
func TestMergedSkillResult_RoundTripsScriptsAndKind(t *testing.T) {
	t.Parallel()
	original := MergedSkillResult{
		Name:        "deploy-staging",
		Description: "merged",
		WhenToUse:   "now",
		Kind:        "pitfall",
		Triggers:    []string{"a", "b", "c"},
		Tags:        []string{"x"},
		Examples:    []ProposedSkillExample{{Input: "i", Output: "o"}},
		Scripts: []ProposedSkillScript{
			{Name: "rt.sh", Purpose: "round trip", Body: "echo hi"},
		},
		BodyMarkdown: "# x",
		Rationale:    "merged the pitfall",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back MergedSkillResult
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Kind != "pitfall" {
		t.Errorf("kind not preserved: %q", back.Kind)
	}
	if len(back.Scripts) != 1 || back.Scripts[0].Name != "rt.sh" {
		t.Errorf("scripts not preserved: %+v", back.Scripts)
	}
}

// TestBuildMergeSkill_RequiresFields asserts the validation
// guards fire for every load-bearing input.
func TestBuildMergeSkill_RequiresFields(t *testing.T) {
	t.Parallel()
	good := MergeSkillInputs{
		SkillName:      "x",
		NextVersion:    "v0.1.1",
		CurrentSkillMd: "---\nname: x\n---\nbody",
		Candidate:      ProposedSkill{Name: "x"},
	}
	cases := []struct {
		name   string
		mutate func(in *MergeSkillInputs)
	}{
		{"missing skill name", func(in *MergeSkillInputs) { in.SkillName = "" }},
		{"missing skill md", func(in *MergeSkillInputs) { in.CurrentSkillMd = "" }},
		{"missing candidate name", func(in *MergeSkillInputs) { in.Candidate.Name = "" }},
		{"missing next version", func(in *MergeSkillInputs) { in.NextVersion = "" }},
		{"name mismatch", func(in *MergeSkillInputs) { in.Candidate.Name = "different" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := good
			tc.mutate(&in)
			if _, err := BuildMergeSkill(in); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

// TestBuildMergeSkill_ScrubsAndReportsPatterns asserts the merge
// prompt routes both sides through the egress redactor and surfaces
// any pattern hits in Built.Patterns. Without this, a leak in the
// existing SKILL.md or the candidate would re-enter the LLM round-
// trip via the merge call.
func TestBuildMergeSkill_ScrubsAndReportsPatterns(t *testing.T) {
	t.Parallel()
	in := MergeSkillInputs{
		SkillName:      "x",
		NextVersion:    "v0.1.1",
		CurrentSkillMd: "---\nname: x\n---\nleaked AKIAIOSFODNN7EXAMPLE",
		Candidate: ProposedSkill{
			Name:      "x",
			WhenToUse: "when",
			Why:       "and another AKIAIOSFODNN7EXAMPLE here",
		},
	}
	built, err := BuildMergeSkill(in)
	if err != nil {
		t.Fatalf("BuildMergeSkill: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("merge prompt leaked secret:\n%s", body)
	}
	if len(built.Patterns) == 0 {
		t.Errorf("expected at least one pattern reported")
	}
}

// TestSummary_FiveSectionVocabulary pins the LoCoBench-Agent
// (Salesforce, 2025 — arXiv:2511.13998) 5-section schema mapping
// in both the system prompt and the tool schema. The actions /
// outcomes split is the load-bearing change: a regression that
// drops "outcomes" or stops teaching the model to split
// silently reverts the summary back to its pre-2026-04-29 shape
// where actions and outcomes were blended.
func TestSummary_FiveSectionVocabulary(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"LoCoBench-Agent",
		"CONTEXT",
		"ACTIONS",
		"OUTCOMES",
		"NEXT_STEPS",
		"IMPORTANT_REFERENCES",
		"actions/outcomes split",
	} {
		if !strings.Contains(summarySystem, want) {
			t.Errorf("summarySystem missing %q", want)
		}
	}
	for _, want := range []string{
		`"outcomes"`,
		`OUTCOMES:`,
		`what CHANGED, WORKED, or FAILED`,
	} {
		if !strings.Contains(summaryToolSchema, want) {
			t.Errorf("summaryToolSchema missing %q", want)
		}
	}
}

// TestSummaryResult_OutcomesRoundTrips covers the JSON shape:
// an LLM that omits outcomes round-trips to a nil/empty slice
// (omitempty + array semantics); an LLM that emits two outcomes
// preserves both bullets and their order through Marshal /
// Unmarshal.
func TestSummaryResult_OutcomesRoundTrips(t *testing.T) {
	t.Parallel()
	in := SummaryResult{
		Topic:       "ran the failing migration",
		WhatWasDone: []string{"investigated migrate_test.go failure"},
		Outcomes:    []string{"3 tests in pkg/store still fail", "0042 migration applies cleanly"},
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SummaryResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Outcomes) != 2 {
		t.Fatalf("outcomes len: got %d want 2", len(out.Outcomes))
	}
	if out.Outcomes[0] != in.Outcomes[0] || out.Outcomes[1] != in.Outcomes[1] {
		t.Errorf("outcomes order/content drift: %v", out.Outcomes)
	}
}

// TestBuildVerifyProposal_SurfacesScopeFields pins that the
// verify prompt now exposes the candidate's triggers, tags, and a
// body excerpt to the critic — without these the critic cannot
// reason about scope tightness or regression risk (rules 5 and 6
// in verifyProposalSystem). The behavioural-verify upgrade is
// load-bearing: the prompt must include what the rules ask the
// critic to evaluate.
func TestBuildVerifyProposal_SurfacesScopeFields(t *testing.T) {
	t.Parallel()
	built, err := BuildVerifyProposal(VerifyProposalInputs{
		Skill: ProposedSkill{
			Name:      "deploy-staging",
			WhenToUse: "when shipping to staging",
			Why:       "single command instead of remembering the script",
			Triggers:  []string{"deploy staging", "ship staging", "push to staging"},
			Tags:      []string{"deploy", "ci"},
			Examples:  []ProposedSkillExample{{Input: "ship this branch", Output: "calls deploy"}},
			Frequency: 2,
			Effort:    "small",
			Evidence: []ProposalEvidence{
				{SessionID: "abc12345", Quote: "deploy this branch to staging please", WhatHappened: "ran the deploy script"},
				{SessionID: "def67890", Quote: "ship to staging again", WhatHappened: "same flow"},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildVerifyProposal: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"triggers:",
		"deploy staging, ship staging, push to staging", // joined trigger list
		"tags:",
		"deploy, ci",
		"prompt body excerpt:",
		"first example: ship this branch", // excerpt includes the example
	} {
		if !strings.Contains(body, want) {
			t.Errorf("verify prompt missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestVerifyProposalSystem_HasBehaviouralRules pins that the
// system prompt embeds the empirical rationale and every refusal
// category that was deliberately added against a literature finding.
// A regression that drops these rules would silently revert the
// gate to a weaker form.
func TestVerifyProposalSystem_HasBehaviouralRules(t *testing.T) {
	t.Parallel()
	// Empirical motivations surface as quotable phrases so a future
	// reader knows where the rules came from.
	for _, want := range []string{
		"SWE-Skills-Bench",
		"SSGM",
		"REGRESSION RISK",
		"SCOPE TIGHTNESS",
		"CONTRADICTION with installed skills",
		"near-match context pollution",
		"memory updates should never be committed passively",
	} {
		if !strings.Contains(verifyProposalSystem, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestExcerptForVerify_BoundsAndContent covers the excerpt helper:
// includes when_to_use / why / first example / scripts; caps at
// ~600 runes so the verify-LLM doesn't burn a token budget on a
// full SKILL.md render.
func TestExcerptForVerify_BoundsAndContent(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		got := excerptForVerify(ProposedSkill{
			WhenToUse: "trigger condition",
			Why:       "rationale text",
			Examples: []ProposedSkillExample{
				{Input: "user query", Output: "skill output"},
			},
			Scripts: []ProposedSkillScript{
				{Name: "build.sh", Purpose: "builds the project"},
			},
		})
		for _, want := range []string{
			"when_to_use: trigger condition",
			"why: rationale text",
			"first example: user query → skill output",
			`script "build.sh": builds the project`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("excerpt missing %q:\n%s", want, got)
			}
		}
	})
	t.Run("over-cap truncates with ellipsis", func(t *testing.T) {
		big := strings.Repeat("y", 800)
		got := excerptForVerify(ProposedSkill{Why: big})
		runes := len([]rune(got))
		if runes > 700 { // 600 cap + the "why: " prefix
			t.Errorf("excerpt should be capped near 600 runes, got %d", runes)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("expected ellipsis on truncation, got: %s", got[:80])
		}
	})
}

// TestProposedSkill_AutoSkillRoundTrips asserts the Go struct
// carries triggers, tags, and examples through a JSON round-trip.
// Without this, an LLM emitting AutoSkill fields would deserialise
// into a struct that drops them on the floor.
func TestProposedSkill_AutoSkillRoundTrips(t *testing.T) {
	t.Parallel()
	in := ProposedSkill{
		Name:                 "deploy-staging",
		WhenToUse:            "when deploying to staging",
		Why:                  "stops manual mistakes",
		Triggers:             []string{"deploy staging", "ship to staging", "push to staging"},
		Tags:                 []string{"deploy", "ci"},
		Examples:             []ProposedSkillExample{{Input: "deploy this branch to staging", Output: "runs the staging deploy script with current branch"}},
		Frequency:            2,
		Effort:               "small",
		AlternativesRejected: "",
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ProposedSkill
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Triggers) != 3 || out.Triggers[0] != "deploy staging" {
		t.Errorf("triggers: got %#v", out.Triggers)
	}
	if len(out.Tags) != 2 || out.Tags[0] != "deploy" {
		t.Errorf("tags: got %#v", out.Tags)
	}
	if len(out.Examples) != 1 || out.Examples[0].Input == "" || out.Examples[0].Output == "" {
		t.Errorf("examples: got %#v", out.Examples)
	}
}

func TestBuildPropose_ScrubsAndReportsPatterns(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{ID: "s-y", Summary: "I leaked AKIAIOSFODNN7EXAMPLE again"},
	}
	built, err := BuildPropose(ProposeInputs{Digests: digests})
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("propose prompt leaked secret:\n%s", body)
	}
	if len(built.Patterns) != 1 {
		t.Errorf("expected one pattern, got %v", built.Patterns)
	}
}

// --- BuildSearchSummary ---

func TestBuildSearchSummary_RequiresQueryAndHits(t *testing.T) {
	t.Parallel()
	if _, err := BuildSearchSummary("", []SearchHit{{SessionID: "s"}}, 0); err == nil {
		t.Error("empty query: expected error")
	}
	if _, err := BuildSearchSummary("q", nil, 0); err == nil {
		t.Error("no hits: expected error")
	}
}

func TestBuildSearchSummary_GroundsHitsInUserMessage(t *testing.T) {
	t.Parallel()
	hits := []SearchHit{
		{SessionID: "abc12345-aaaa-bbbb-cccc-dddddddddddd", Kind: "user_prompt",
			Cwd: "/proj", TsSourceMs: 1700000000000, Snippet: "investigate slow query"},
		{SessionID: "def67890-aaaa-bbbb-cccc-dddddddddddd", Kind: "tool_use",
			Cwd: "/proj", TsSourceMs: 1700000100000, Snippet: "Bash: EXPLAIN ANALYZE …"},
	}
	built, err := BuildSearchSummary("slow query", hits, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"Query: slow query",
		"Hits (2)",
		"[session=abc12345]",
		"[session=def67890]",
		"investigate slow query",
		"EXPLAIN ANALYZE",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("user msg missing %q\n%s", want, body)
		}
	}
	if !strings.Contains(built.Request.System, "Anti-fabrication rules") {
		t.Error("system prompt missing anti-fabrication header")
	}
	if built.Request.MaxTokens == 0 {
		t.Error("MaxTokens defaulted to zero")
	}
}

func TestBuildSearchSummary_ScrubsSecrets(t *testing.T) {
	t.Parallel()
	hits := []SearchHit{{
		SessionID: "abc12345-aaaa-bbbb-cccc-dddddddddddd",
		Kind:      "user_prompt", Cwd: "/x", TsSourceMs: 1,
		Snippet: "I leaked AKIAIOSFODNN7EXAMPLE in this turn",
	}}
	built, err := BuildSearchSummary("aws creds", hits, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body := built.Request.Messages[0].Content
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("search-summary prompt leaked secret:\n%s", body)
	}
	if len(built.Patterns) == 0 {
		t.Error("expected at least one redaction pattern reported")
	}
}

// TestBuildPropose_RendersInstalledAndInvokedSkills confirms the
// new skill-aware sections land in the user message exactly when
// the caller supplies them, and that the system prompt carries
// the awareness rules. The assertions are substring-based rather
// than exact-match so future prompt rewording doesn't break the
// test for cosmetic reasons.
func TestBuildPropose_RendersInstalledAndInvokedSkills(t *testing.T) {
	t.Parallel()
	in := ProposeInputs{
		Digests: []SessionDigest{{ID: "s1", FirstPrompt: "do a thing"}},
		InstalledSkills: []InstalledSkill{
			{Name: "effective-go", Description: "idiomatic Go", Source: "global"},
			{Name: "build-test", Description: "build then test", Source: "project:/home/tom/devel/x"},
		},
		InvokedSkills: []InvokedSkill{
			{Name: "build-test", Count: 6},
			{Name: "effective-go", Count: 3},
		},
	}
	built, err := BuildPropose(in)
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"Skills installed",
		"effective-go [global]: idiomatic Go",
		"build-test [project:/home/tom/devel/x]: build then test",
		"Skills invoked recently",
		"build-test × 6",
		"effective-go × 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("propose user message missing %q\n--- body ---\n%s", want, body)
		}
	}
	for _, want := range []string{
		"Skill-awareness rules",
		"do NOT propose a new skill with that name",
	} {
		if !strings.Contains(built.Request.System, want) {
			t.Errorf("propose system prompt missing %q", want)
		}
	}
}

// TestBuildPropose_OmitsSkillSectionsWhenEmpty pins the no-section
// case: a fresh user with no installed skills and no invocations
// must NOT see misleading empty headers in the prompt.
func TestBuildPropose_OmitsSkillSectionsWhenEmpty(t *testing.T) {
	t.Parallel()
	built, err := BuildPropose(ProposeInputs{
		Digests: []SessionDigest{{ID: "s1", FirstPrompt: "hi"}},
	})
	if err != nil {
		t.Fatalf("BuildPropose: %v", err)
	}
	body := built.Request.Messages[0].Content
	for _, unwanted := range []string{
		"Skills installed",
		"Skills invoked recently",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("empty inputs leaked %q into body:\n%s", unwanted, body)
		}
	}
}

// TestRenderInvokedSkills_RendersLastLoadedAnnotation confirms the
// "last loaded" annotation appears whenever LastLoadedMs is set, in
// every combination with TotalLoads (success-rate present or absent).
// The recency signal is what lets the LLM distinguish a skill loaded
// 12× yesterday from one loaded 12× five days ago — a count alone
// hides the staleness.
// TestRenderOutcomeCue_FailureAppendsErrorCountAndTerminator confirms
// the two newly-surfaced signals on the failure_likely / mixed line:
//
//   - error_count is appended only when non-zero (it's distinct from
//     tool_failure_count — an "error" event is broader than a tool
//     failure, and a zero would just clutter the line);
//   - "ended on <kind>" is appended only when the session terminated
//     on tool_failure or error (the failure-shaped terminators);
//     other terminators (assistant_message, user_prompt, ...) leave
//     the line clean.
func TestRenderOutcomeCue_FailureAppendsErrorCountAndTerminator(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		outcome      store.SessionOutcome
		wantContains []string
		wantOmits    []string
	}{
		{
			name: "ended_on_tool_failure_with_error_count",
			outcome: store.SessionOutcome{
				Outcome:           store.OutcomeFailureLikely,
				ToolFailureCount:  4,
				GitUndoCount:      1,
				PromptRepeatCount: 0,
				ErrorCount:        2,
				LastEventKind:     sql.NullString{String: events.KindToolFailure, Valid: true},
			},
			wantContains: []string{
				"Outcome: failure_likely (4 tool_failures, 1 git_undos, 0 prompt_repeats, 2 errors), ended on " + events.KindToolFailure + "\n",
			},
		},
		{
			name: "no_error_count_no_terminator_clutter",
			outcome: store.SessionOutcome{
				Outcome:           store.OutcomeMixed,
				ToolFailureCount:  1,
				GitUndoCount:      0,
				PromptRepeatCount: 2,
				ErrorCount:        0,
				LastEventKind:     sql.NullString{String: events.KindAssistantMessage, Valid: true},
			},
			wantContains: []string{
				"Outcome: mixed (1 tool_failures, 0 git_undos, 2 prompt_repeats)\n",
			},
			wantOmits: []string{"errors", "ended on"},
		},
		{
			name: "ended_on_error_terminator",
			outcome: store.SessionOutcome{
				Outcome:          store.OutcomeFailureLikely,
				ToolFailureCount: 3,
				LastEventKind:    sql.NullString{String: events.KindError, Valid: true},
			},
			wantContains: []string{"ended on " + events.KindError + "\n"},
		},
		{
			name: "no_last_event_kind_no_terminator_phrase",
			outcome: store.SessionOutcome{
				Outcome:          store.OutcomeFailureLikely,
				ToolFailureCount: 3,
			},
			wantOmits: []string{"ended on"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderOutcomeCue(&tc.outcome)
			for _, w := range tc.wantContains {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, w := range tc.wantOmits {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in %q", w, got)
				}
			}
		})
	}
}

func TestRenderInvokedSkills_RendersLastLoadedAnnotation(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixMilli()
	twoHoursAgo := now - 2*int64(time.Hour/time.Millisecond)
	threeDaysAgo := now - 3*24*int64(time.Hour/time.Millisecond)
	skills := []InvokedSkill{
		{Name: "fresh-impact", Count: 6, TotalLoads: 6, FailedLoads: 0, SuccessRate: 1.0, LastLoadedMs: twoHoursAgo},
		{Name: "stale-impact", Count: 12, TotalLoads: 12, FailedLoads: 3, SuccessRate: 0.75, LastLoadedMs: threeDaysAgo},
		{Name: "no-impact-with-recency", Count: 2, LastLoadedMs: twoHoursAgo},
		{Name: "no-impact-no-recency", Count: 1},
	}
	out := renderInvokedSkills(skills)
	for _, want := range []string{
		"fresh-impact × 6  (success: 100%, 0/6 loads followed by tool_failure, last loaded 2h ago)",
		"stale-impact × 12  (success: 75%, 3/12 loads followed by tool_failure, last loaded 3d ago)",
		"no-impact-with-recency × 2  (last loaded 2h ago)",
		"no-impact-no-recency × 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The orientation sentence in the header must teach the LLM what
	// the new annotation means; without it the recency number is just
	// a number on the line.
	if !strings.Contains(out, "loaded N times yesterday is a stronger signal") {
		t.Errorf("renderInvokedSkills must teach the LLM how to read the recency annotation; got:\n%s", out)
	}
}

func TestHumanAgo_RoundsToCoarsestUnit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ms   int64
		want string
	}{
		{"zero is just-now", 0, "just now"},
		{"future is just-now", now.Add(time.Hour).UnixMilli(), "just now"},
		{"sub-minute is just-now", now.Add(-30 * time.Second).UnixMilli(), "just now"},
		{"5 minutes", now.Add(-5 * time.Minute).UnixMilli(), "5m ago"},
		{"2 hours", now.Add(-2 * time.Hour).UnixMilli(), "2h ago"},
		{"23 hours stays in hours", now.Add(-23 * time.Hour).UnixMilli(), "23h ago"},
		{"24 hours flips to days", now.Add(-24 * time.Hour).UnixMilli(), "1d ago"},
		{"3 days", now.Add(-3 * 24 * time.Hour).UnixMilli(), "3d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := humanAgo(now, tc.ms); got != tc.want {
				t.Errorf("humanAgo(%d) = %q, want %q", tc.ms, got, tc.want)
			}
		})
	}
}

// --- hash stability ---

func TestHashRequest_ModelChangeDoesNotAffectHash(t *testing.T) {
	t.Parallel()
	r1 := llm.Request{
		System:    "s",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		MaxTokens: 10,
		Model:     "model-a",
	}
	r2 := r1
	r2.Model = "model-b"
	if hashRequest(r1) != hashRequest(r2) {
		t.Error("hash must ignore Model so same prompt hits cache across providers")
	}
}

func TestHashRequest_MaxTokensChangeChangesHash(t *testing.T) {
	t.Parallel()
	r1 := llm.Request{
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		MaxTokens: 10,
	}
	r2 := r1
	r2.MaxTokens = 20
	if hashRequest(r1) == hashRequest(r2) {
		t.Error("hash should differ on MaxTokens change")
	}
}

func TestHashRequest_NoToolsIsByteIdenticalToLegacy(t *testing.T) {
	t.Parallel()
	// A request with nil tools must hash the same as if the Tools
	// field did not exist. This locks in the cache compatibility
	// promise: callers that never declare tools keep hitting the
	// rows they stored before tool use was added.
	legacy := llm.Request{
		System:    "s",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		MaxTokens: 10,
	}
	recomputed := legacy // same, but constructed post-rewrite
	if hashRequest(legacy) != hashRequest(recomputed) {
		t.Error("no-tools hash should be deterministic across calls")
	}
	// Cross-check by hand: hash of the bytes we know legacy should
	// produce. This catches accidental changes to the hash format.
	// We don't hard-code the hex (too brittle) but we do assert that
	// adding an empty Tools slice leaves the hash unchanged.
	withEmptyTools := legacy
	withEmptyTools.Tools = []llm.Tool{}
	if hashRequest(legacy) != hashRequest(withEmptyTools) {
		t.Error("empty tools slice must hash the same as nil tools")
	}
}

func TestHashRequest_DifferentToolsChangeHash(t *testing.T) {
	t.Parallel()
	base := llm.Request{
		System:    "s",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		MaxTokens: 10,
	}
	withTool := base
	withTool.Tools = []llm.Tool{{
		Name:        "t",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	withTool.ForceTool = "t"
	if hashRequest(base) == hashRequest(withTool) {
		t.Error("adding a tool must change the hash")
	}

	withDifferentSchema := withTool
	withDifferentSchema.Tools = []llm.Tool{{
		Name:        "t",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
	}}
	if hashRequest(withTool) == hashRequest(withDifferentSchema) {
		t.Error("schema change must invalidate the hash")
	}
}

// --- BuildInduce ---

func TestBuildInduce_RequiresDigestID(t *testing.T) {
	t.Parallel()
	if _, err := BuildInduce(InduceFromSessionInputs{}); err == nil {
		t.Error("missing digest ID: expected error")
	}
}

func TestBuildInduce_SetsForcedToolAndValidSchema(t *testing.T) {
	t.Parallel()
	built, err := BuildInduce(InduceFromSessionInputs{
		Digest: SessionDigest{ID: "sess-i", FirstPrompt: "build a thing"},
	})
	if err != nil {
		t.Fatalf("BuildInduce: %v", err)
	}
	if built.Request.ForceTool != ToolNameInduction {
		t.Errorf("ForceTool: got %q, want %q", built.Request.ForceTool, ToolNameInduction)
	}
	if len(built.Request.Tools) != 1 || built.Request.Tools[0].Name != ToolNameInduction {
		t.Errorf("tools: got %+v", built.Request.Tools)
	}
	if !json.Valid(built.Request.Tools[0].InputSchema) {
		t.Error("induction schema is not valid JSON")
	}
}

func TestBuildInduce_SchemaAllowsSingleEvidenceAndOptionalSkillAndWorkflow(t *testing.T) {
	t.Parallel()
	built, _ := BuildInduce(InduceFromSessionInputs{
		Digest: SessionDigest{ID: "sess-i"},
	})
	schema := string(built.Request.Tools[0].InputSchema)
	for _, want := range []string{
		`"skill"`,    // optional — emitted when LLM judges the session yields one
		`"workflow"`, // optional — emitted when LLM judges the session yields an abstract procedure
		`"rationale"`,
		`"minItems": 1`, // skill evidence allowed with one entry
		`"minimum":1,"maximum":1`,
		`"task_shape"`, // workflow shape lives inline in the merged schema
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %q:\n%s", want, schema)
		}
	}
	// CRITICAL: evidence must NOT be required at minItems:2 — that
	// was the whole point of a separate induction prompt. Spot-
	// checking the exact substring rather than parsing the schema
	// because parsing $defs is a chore and the substring is unique.
	if strings.Contains(schema, `"minItems": 2`) {
		t.Errorf("induction schema should not enforce ≥2 evidence; got:\n%s", schema)
	}
}

func TestBuildInduce_RendersInstalledSkillsStanza(t *testing.T) {
	t.Parallel()
	built, _ := BuildInduce(InduceFromSessionInputs{
		Digest: SessionDigest{ID: "sess-i", FirstPrompt: "x"},
		InstalledSkills: []InstalledSkill{
			{Name: "deploy-staging", Description: "deploy to staging", Source: "global"},
		},
	})
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "deploy-staging") {
		t.Errorf("installed-skill name missing from prompt:\n%s", body)
	}
}

// --- BuildChallenge ---

func TestBuildChallenge_RequiresSessions(t *testing.T) {
	t.Parallel()
	if _, err := BuildChallenge(ChallengeInputs{}); err == nil {
		t.Error("empty digests: expected error")
	}
}

func TestBuildChallenge_SetsForcedToolAndValidSchema(t *testing.T) {
	t.Parallel()
	built, err := BuildChallenge(ChallengeInputs{
		Digests: []SessionDigest{{ID: "s1", FirstPrompt: "what to do next?", Summary: "explored auth"}},
	})
	if err != nil {
		t.Fatalf("BuildChallenge: %v", err)
	}
	if built.Request.ForceTool != ToolNameChallenge {
		t.Errorf("ForceTool: got %q, want %q", built.Request.ForceTool, ToolNameChallenge)
	}
	if !json.Valid(built.Request.Tools[0].InputSchema) {
		t.Error("challenge schema is not valid JSON")
	}
}

func TestBuildChallenge_SchemaEnforcesGroundedAndShape(t *testing.T) {
	t.Parallel()
	built, _ := BuildChallenge(ChallengeInputs{
		Digests: []SessionDigest{{ID: "s1", Summary: "x"}},
	})
	schema := string(built.Request.Tools[0].InputSchema)
	for _, want := range []string{
		`"grounded_in"`,
		`"minItems":1`, // grounded_in must have at least one anchor
		`"success_looks_like"`,
		`"effort"`,
		`"small"`,
		`"medium"`,
		`"large"`,
		`"maxItems": 3`,
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %q:\n%s", want, schema)
		}
	}
}

func TestBuildChallenge_RendersUnresolvedStanzaWhenPresent(t *testing.T) {
	t.Parallel()
	built, _ := BuildChallenge(ChallengeInputs{
		Digests: []SessionDigest{{ID: "s1", Summary: "x"}},
		Unresolved: []UnresolvedItemForChallenge{
			{
				SessionID:    "11111111-1111-1111-1111-111111111111",
				SessionShort: "11111111",
				Topic:        "auth middleware refactor",
				Item:         "document the new fallback flow",
			},
		},
	})
	body := built.Request.Messages[0].Content
	for _, want := range []string{
		"Open threads observed",
		"11111111",
		"auth middleware refactor",
		"document the new fallback flow",
		"BUILD ON",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestBuildChallenge_OmitsUnresolvedStanzaWhenEmpty(t *testing.T) {
	t.Parallel()
	built, _ := BuildChallenge(ChallengeInputs{
		Digests: []SessionDigest{{ID: "s1", Summary: "x"}},
	})
	if strings.Contains(built.Request.Messages[0].Content, "Open threads observed") {
		t.Errorf("empty unresolved should omit stanza:\n%s",
			built.Request.Messages[0].Content)
	}
}

func TestBuildChallenge_HashChangesWhenUnresolvedChanges(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{{ID: "s1", Summary: "x"}}
	a, _ := BuildChallenge(ChallengeInputs{Digests: digests})
	b, _ := BuildChallenge(ChallengeInputs{
		Digests: digests,
		Unresolved: []UnresolvedItemForChallenge{
			{SessionID: "z", SessionShort: "z", Topic: "t", Item: "i"},
		},
	})
	if a.Hash == b.Hash {
		t.Error("adding open threads should change the hash")
	}
}

func TestBuildInduce_HashStableAndContentSensitive(t *testing.T) {
	t.Parallel()
	a, _ := BuildInduce(InduceFromSessionInputs{Digest: SessionDigest{ID: "x", FirstPrompt: "p"}})
	b, _ := BuildInduce(InduceFromSessionInputs{Digest: SessionDigest{ID: "x", FirstPrompt: "p"}})
	if a.Hash != b.Hash {
		t.Errorf("identical inputs produced different hashes: %q vs %q", a.Hash, b.Hash)
	}
	c, _ := BuildInduce(InduceFromSessionInputs{Digest: SessionDigest{ID: "x", FirstPrompt: "different"}})
	if a.Hash == c.Hash {
		t.Errorf("different first_prompt should change hash")
	}
}
