package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadClaudeFixture reads a single-line transcript fixture and parses
// it as a claudeEntry for direct classification tests. Distinct name
// avoids collision with loadFixture in assemble_test.go.
func loadClaudeFixture(t *testing.T, name string) ([]byte, *claudeEntry) {
	t.Helper()
	path := filepath.Join("testdata", "claude_transcripts", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	raw = bytes.TrimSpace(raw)
	var entry claudeEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return raw, &entry
}

// --- classification tests (per-type) ---

func TestClassify_UserPromptString(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "user_prompt_string.jsonl")
	kind, role, content, tool := classifyClaudeEntry(entry)
	if kind != "user_prompt" {
		t.Errorf("kind: got %q, want user_prompt", kind)
	}
	if role != "user" {
		t.Errorf("role: got %q", role)
	}
	if content != "what is jsonl format" {
		t.Errorf("content: got %q", content)
	}
	if tool != nil {
		t.Errorf("tool should be nil: %+v", tool)
	}
}

func TestClassify_UserToolResult(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "user_tool_result.jsonl")
	kind, role, content, tool := classifyClaudeEntry(entry)
	if kind != "tool_result" {
		t.Errorf("kind: got %q, want tool_result", kind)
	}
	if role != "tool" {
		t.Errorf("role: got %q", role)
	}
	if content != "file contents here" {
		t.Errorf("content: got %q", content)
	}
	if tool == nil || tool.CallID != "toolu_test01" {
		t.Errorf("tool.call_id: got %+v, want toolu_test01", tool)
	}
}

func TestClassify_AssistantText(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "assistant_text.jsonl")
	kind, role, content, tool := classifyClaudeEntry(entry)
	if kind != "assistant_message" {
		t.Errorf("kind: got %q", kind)
	}
	if role != "assistant" {
		t.Errorf("role: got %q", role)
	}
	if content != "JSON Lines is one object per line." {
		t.Errorf("content: got %q", content)
	}
	if tool != nil {
		t.Errorf("tool should be nil for text-only: %+v", tool)
	}
}

func TestClassify_AssistantToolUse(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "assistant_tool_use.jsonl")
	kind, role, content, tool := classifyClaudeEntry(entry)
	if kind != "tool_use" {
		t.Errorf("kind: got %q, want tool_use", kind)
	}
	if role != "tool" {
		t.Errorf("role: got %q", role)
	}
	if content != "Read" {
		t.Errorf("content should be tool name: got %q", content)
	}
	if tool == nil || tool.Name != "Read" || tool.CallID != "toolu_test02" {
		t.Errorf("tool: got %+v", tool)
	}
}

func TestClassify_System(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "system.jsonl")
	kind, role, _, _ := classifyClaudeEntry(entry)
	if kind != "system_message" {
		t.Errorf("kind: got %q", kind)
	}
	if role != "system" {
		t.Errorf("role: got %q", role)
	}
}

// --- envelope conversion tests ---

func TestTranscriptEntryToEnvelope_UUIDFlowsToEventID(t *testing.T) {
	t.Parallel()
	raw, entry := loadClaudeFixture(t, "user_prompt_string.jsonl")
	env, out, err := transcriptEntryToEnvelope(entry, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if env.EventID != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("EventID: got %q, want uuid from fixture", env.EventID)
	}
	if env.SourceAgent != "claude-code" {
		t.Errorf("SourceAgent: got %q", env.SourceAgent)
	}
	if env.SourceAgentVersion != "2.0.0" {
		t.Errorf("Version: got %q", env.SourceAgentVersion)
	}
	if env.Transport != "import" {
		t.Errorf("Transport: got %q, want import", env.Transport)
	}
	if env.Cwd != "/home/user/project" {
		t.Errorf("Cwd: got %q", env.Cwd)
	}
	if env.Payload["uuid"] != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("raw payload not preserved: %v", env.Payload)
	}
	if err := env.Validate(); err != nil {
		t.Errorf("assembled envelope failed validate: %v", err)
	}
	if len(out) == 0 {
		t.Error("out bytes should be the marshalled envelope")
	}
}

func TestTranscriptEntryToEnvelope_AppliesRedaction(t *testing.T) {
	t.Parallel()
	raw, entry := loadClaudeFixture(t, "user_prompt_string.jsonl")

	// Inject a synthetic secret in the transcript entry's text fields
	// without writing a new fixture. The goal is to prove the envelope
	// builder scrubs before marshaling — both the in-memory envelope
	// and the bytes handed to IngestEnvelope must be secret-free.
	secret := "AKIAIOSFODNN7EXAMPLE"
	entry.CWD = "/home/" + secret + "/project"

	env, out, err := transcriptEntryToEnvelope(entry, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("envelope must carry Redaction.Applied=true")
	}
	if strings.Contains(env.Cwd, secret) {
		t.Errorf("cwd carries secret: %q", env.Cwd)
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("marshaled bytes carry secret")
	}
}

func TestTranscriptEntryToEnvelope_MissingSessionIDIsError(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "user_prompt_string.jsonl")
	entry.SessionID = ""
	_, _, err := transcriptEntryToEnvelope(entry, []byte("{}"))
	if err == nil {
		t.Error("expected error for missing sessionId")
	}
}

func TestTranscriptEntryToEnvelope_MalformedTimestampIsError(t *testing.T) {
	t.Parallel()
	_, entry := loadClaudeFixture(t, "user_prompt_string.jsonl")
	entry.Timestamp = "not-a-date"
	_, _, err := transcriptEntryToEnvelope(entry, []byte("{}"))
	if err == nil {
		t.Error("expected error for bad timestamp")
	}
}

// --- file-level import tests ---

func TestImportClaudeTranscripts_SingleFile(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var warns bytes.Buffer

	path := filepath.Join("testdata", "claude_transcripts", "user_prompt_string.jsonl")
	report, err := ImportClaudeTranscripts(path, s, &warns)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.FilesRead != 1 {
		t.Errorf("FilesRead: got %d, want 1", report.FilesRead)
	}
	if report.Imported != 1 {
		t.Errorf("Imported: got %d, want 1", report.Imported)
	}
	if warns.Len() != 0 {
		t.Errorf("unexpected warnings: %s", warns.String())
	}

	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 1 {
		t.Errorf("events count: got %d, want 1", n)
	}
}

func TestImportClaudeTranscripts_MixedSessionCountsCorrectly(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var warns bytes.Buffer

	path := filepath.Join("testdata", "claude_transcripts", "mixed_session.jsonl")
	report, err := ImportClaudeTranscripts(path, s, &warns)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// mixed_session.jsonl layout (7 lines):
	//   1 user (canonical, ok)               → imported
	//   1 file-history-snapshot (internal)   → silent skip
	//   1 queue-operation (internal)         → silent skip
	//   1 assistant (canonical, ok)          → imported
	//   1 permission-mode (internal)         → silent skip
	//   1 attachment (internal)              → silent skip
	//   1 user with empty uuid (canonical)   → skipped_missing_uuid
	if report.LinesRead != 7 {
		t.Errorf("LinesRead: got %d, want 7", report.LinesRead)
	}
	if report.Imported != 2 {
		t.Errorf("Imported: got %d, want 2", report.Imported)
	}
	if report.SkippedMissingUUID != 1 {
		t.Errorf("SkippedMissingUUID: got %d, want 1", report.SkippedMissingUUID)
	}
	if report.Invalid != 0 {
		t.Errorf("Invalid: got %d, want 0", report.Invalid)
	}

	// Missing-uuid row should have a loud warning
	if !strings.Contains(warns.String(), "without uuid") {
		t.Errorf("expected 'without uuid' warning in stderr:\n%s", warns.String())
	}
}

func TestImportClaudeTranscripts_IdempotentReplay(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var warns bytes.Buffer

	path := filepath.Join("testdata", "claude_transcripts", "mixed_session.jsonl")
	if _, err := ImportClaudeTranscripts(path, s, &warns); err != nil {
		t.Fatalf("first: %v", err)
	}

	second, err := ImportClaudeTranscripts(path, s, &warns)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Imported != 0 {
		t.Errorf("replay should import 0, got %d", second.Imported)
	}
	if second.Deduped != 2 {
		t.Errorf("replay should dedupe 2, got %d", second.Deduped)
	}
}

func TestImportClaudeTranscripts_DirectoryWalk(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var warns bytes.Buffer

	dir := filepath.Join("testdata", "claude_transcripts")
	report, err := ImportClaudeTranscripts(dir, s, &warns)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// Six canonical single-line fixtures (all types) + mixed_session.jsonl's
	// two canonical rows = 8 imported. Real count is whatever lands; we
	// just check it's >= 7 to stay robust against future fixture adds.
	if report.Imported < 7 {
		t.Errorf("Imported: got %d, want >= 7", report.Imported)
	}
	if report.FilesRead < 6 {
		t.Errorf("FilesRead: got %d, want >= 6", report.FilesRead)
	}
}

func TestImportClaudeTranscripts_NonExistentPathReturnsError(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, err := ImportClaudeTranscripts(filepath.Join(t.TempDir(), "nope"), s, &bytes.Buffer{})
	if err == nil {
		t.Error("expected error for non-existent path")
	}
}

func TestImportClaudeTranscripts_MalformedJSONIsCountedNotFatal(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var warns bytes.Buffer

	// Write a temp file with two lines: one good, one bad JSON.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	good, _ := os.ReadFile(filepath.Join("testdata", "claude_transcripts", "user_prompt_string.jsonl"))
	content := append(append([]byte(nil), good...), []byte("\n{not json\n")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	report, err := ImportClaudeTranscripts(path, s, &warns)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Imported != 1 {
		t.Errorf("Imported: got %d, want 1", report.Imported)
	}
	if report.Invalid != 1 {
		t.Errorf("Invalid: got %d, want 1", report.Invalid)
	}
	if !strings.Contains(warns.String(), "invalid JSON") {
		t.Errorf("expected invalid-JSON warning, got: %s", warns.String())
	}
}

func TestReport_StringShape(t *testing.T) {
	t.Parallel()
	r := ClaudeImportReport{
		FilesRead: 3, LinesRead: 20, Imported: 15,
		Deduped: 2, SkippedMissingUUID: 1, Invalid: 2,
		DurationMS: 42,
	}
	s := r.String()
	for _, want := range []string{
		"files read:           3",
		"imported:             15",
		"deduped:              2",
		"skipped missing uuid: 1",
		"invalid:              2",
		"duration_ms:          42",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in report:\n%s", want, s)
		}
	}
}
