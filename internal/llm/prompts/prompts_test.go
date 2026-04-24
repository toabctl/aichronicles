package prompts

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
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
	built, err := BuildSummary("sess-abc", events)
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

func TestBuildSummary_ScrubsSecretsAndReportsPatterns(t *testing.T) {
	t.Parallel()
	// Event content carrying a synthetic AWS key. The egress scrub
	// here is our last line of defense — a stored event missed by
	// ingest-time redaction must not reach the LLM raw.
	events := []store.EventView{
		{EventID: "e1", Kind: "user_prompt", Role: nullS("user"),
			ContentText: nullS("my AKIAIOSFODNN7EXAMPLE leak"), TsSourceMs: 1},
	}
	built, err := BuildSummary("sess-x", events)
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
	if _, err := BuildSummary("", []store.EventView{{}}); err == nil {
		t.Error("empty sessionID: expected error")
	}
	if _, err := BuildSummary("sess", nil); err == nil {
		t.Error("nil events: expected error")
	}
}

func TestBuildSummary_HashDeterministicSameInput(t *testing.T) {
	t.Parallel()
	events := []store.EventView{
		{Kind: "user_prompt", ContentText: nullS("identical prompt"), TsSourceMs: 1},
	}
	a, _ := BuildSummary("sess", events)
	b, _ := BuildSummary("sess", events)
	if a.Hash != b.Hash {
		t.Errorf("hash not deterministic: %q vs %q", a.Hash, b.Hash)
	}
}

func TestBuildSummary_HashChangesWhenContentChanges(t *testing.T) {
	t.Parallel()
	a, _ := BuildSummary("sess", []store.EventView{
		{Kind: "user_prompt", ContentText: nullS("first"), TsSourceMs: 1},
	})
	b, _ := BuildSummary("sess", []store.EventView{
		{Kind: "user_prompt", ContentText: nullS("second"), TsSourceMs: 1},
	})
	if a.Hash == b.Hash {
		t.Errorf("hash unchanged for different content: %q", a.Hash)
	}
}

func TestBuildSummary_NullToolUseEventDoesNotPanic(t *testing.T) {
	t.Parallel()
	// A tool_use event where content_text is NULL but tool_name is
	// set — historically a shape that some importers produce. Must
	// render without panicking; the tool label should still appear.
	events := []store.EventView{
		{
			EventID: "e1", Kind: "tool_use",
			Role:        sql.NullString{},
			ContentText: sql.NullString{},
			TsSourceMs:  1,
			ToolName:    nullS("Bash"),
		},
	}
	built, err := BuildSummary("sess-null", events)
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
	// Every nullable column NULL. The builder should still emit a
	// [kind] header so the transcript stays well-formed.
	events := []store.EventView{
		{
			EventID: "e1", Kind: "session_start",
			TsSourceMs: 1,
		},
	}
	built, err := BuildSummary("sess-bare", events)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	body := built.Request.Messages[0].Content
	if !strings.Contains(body, "[session_start]") {
		t.Errorf("bare event missing kind label:\n%s", body)
	}
}

// --- BuildReflect ---

func TestBuildReflect_RequiresSessions(t *testing.T) {
	t.Parallel()
	if _, err := BuildReflect(nil, 24*time.Hour); err == nil {
		t.Error("empty digests: expected error")
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

func TestBuildReflect_ScrubsSecretsInCwd(t *testing.T) {
	t.Parallel()
	// A pathological Cwd can carry a secret (see ApplyRedaction's
	// rationale for scrubbing env.Cwd). renderDigests must route
	// Cwd through redact.Outbound too.
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
	if _, err := BuildPropose(nil); err == nil {
		t.Error("empty digests: expected error")
	}
}

func TestBuildPropose_ScrubsAndReportsPatterns(t *testing.T) {
	t.Parallel()
	digests := []SessionDigest{
		{ID: "s-y", Summary: "I leaked AKIAIOSFODNN7EXAMPLE again"},
	}
	built, err := BuildPropose(digests)
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
