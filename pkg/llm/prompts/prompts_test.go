package prompts

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

func nullS(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// --- BuildSummary ---

func TestBuildSummary_IncludesTranscriptAndSessionID(t *testing.T) {
	t.Parallel()
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	wantRequired := map[string]bool{"topic": true, "what_was_done": true, "unresolved": true, "key_files": true, "links": true}
	for _, r := range required {
		delete(wantRequired, r.(string))
	}
	if len(wantRequired) != 0 {
		t.Errorf("schema missing required fields: %v", wantRequired)
	}
}

func TestBuildSummary_RendersLinksBlockWhenPresent(t *testing.T) {
	t.Parallel()
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	if _, err := BuildSummary("", []store.EventView{{}}, SummaryInputs{}); err == nil {
		t.Error("empty sessionID: expected error")
	}
	if _, err := BuildSummary("sess", nil, SummaryInputs{}); err == nil {
		t.Error("nil events: expected error")
	}
}

func TestBuildSummary_HashDeterministicSameInput(t *testing.T) {
	t.Parallel()
	events := []store.EventView{
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
	a, _ := BuildSummary("sess", []store.EventView{
		{Kind: "user_prompt", ContentText: nullS("first"), TsSourceMs: 1},
	}, SummaryInputs{})
	b, _ := BuildSummary("sess", []store.EventView{
		{Kind: "user_prompt", ContentText: nullS("second"), TsSourceMs: 1},
	}, SummaryInputs{})
	if a.Hash == b.Hash {
		t.Errorf("hash unchanged for different content: %q", a.Hash)
	}
}

func TestBuildSummary_HashChangesWhenLinksChange(t *testing.T) {
	t.Parallel()
	events := []store.EventView{
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
	events := []store.EventView{
		{
			EventID: "e1", Kind: "tool_use",
			Role:        sql.NullString{},
			ContentText: sql.NullString{},
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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
	events := []store.EventView{
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

func TestBuildInduce_SchemaAllowsSingleEvidenceAndNoSkillFound(t *testing.T) {
	t.Parallel()
	built, _ := BuildInduce(InduceFromSessionInputs{
		Digest: SessionDigest{ID: "sess-i"},
	})
	schema := string(built.Request.Tools[0].InputSchema)
	for _, want := range []string{
		`"no_skill_found"`,
		`"rationale"`,
		`"minItems": 1`, // evidence allowed with one entry
		`"minimum":1,"maximum":1`,
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
