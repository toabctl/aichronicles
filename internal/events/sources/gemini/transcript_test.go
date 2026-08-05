package gemini

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
)

const userMessageFixture = `{
	"sessionId":"sess-1",
	"messages":[
		{
			"id":"01970000-0000-7000-8000-000000000001",
			"timestamp":"2026-05-02T12:00:00.000Z",
			"type":"user",
			"content":[{"text":"hello gemini"}]
		}
	]
}`

const assistantWithToolCallFixture = `{
	"sessionId":"sess-2",
	"messages":[
		{
			"id":"01970000-0000-7000-8000-000000000010",
			"timestamp":"2026-05-02T12:01:00.000Z",
			"type":"gemini",
			"content":"sure I'll run it",
			"model":"gemini-pro",
			"toolCalls":[
				{
					"id":"call-1",
					"name":"run_shell_command",
					"args":{"command":"ls"},
					"status":"success",
					"timestamp":"2026-05-02T12:01:00.500Z",
					"result":[{"functionResponse":{"id":"call-1","name":"run_shell_command","response":{"output":"file1\nfile2"}}}]
				}
			]
		}
	]
}`

// TestTranscriptSource_SecretsAreScrubbedAfterPipelineRedaction is the
// end-to-end gate for the plaintext leak this source used to produce.
//
// The translator stores message content as json.RawMessage and the
// tool call as a *geminiToolCall struct. walkAny's old default arm
// returned both untouched, so ContentText scrubbed cleanly, Applied
// was set to true, the Sink's assertion passed — and the raw secret
// went into raw_envelopes.envelope_json anyway. The scrubbed sibling
// field is exactly what made it invisible.
//
// Asserting on the marshalled envelope rather than on individual
// fields is deliberate: those bytes are what actually get persisted.
func TestTranscriptSource_SecretsAreScrubbedAfterPipelineRedaction(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("a", 36)
	fixture := `{
		"sessionId":"sess-secret",
		"messages":[
			{
				"id":"01970000-0000-7000-8000-000000000001",
				"timestamp":"2026-05-02T12:00:00.000Z",
				"type":"user",
				"content":[{"text":"my token is ` + secret + `"}]
			},
			{
				"id":"01970000-0000-7000-8000-000000000010",
				"timestamp":"2026-05-02T12:01:00.000Z",
				"type":"gemini",
				"content":"running it",
				"toolCalls":[
					{
						"id":"call-1",
						"name":"run_shell_command",
						"args":{"command":"echo ` + secret + `"},
						"status":"success",
						"timestamp":"2026-05-02T12:01:00.500Z",
						"result":[{"functionResponse":{"id":"call-1","name":"run_shell_command",
							"response":{"output":"printed ` + secret + `"}}}]
					}
				]
			}
		]
	}`

	path := writeJSON(t, fixture)
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}
	got := collect(t, src)
	if len(got) == 0 {
		t.Fatal("no events produced")
	}

	for _, evt := range got {
		env := evt.Envelope
		events.ApplyRedaction(env, redact.Default())
		out, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		if strings.Contains(string(out), secret) {
			t.Errorf("kind=%s persisted the raw secret:\n%s", env.Kind, out)
		}
	}
}

func writeJSON(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func collect(t *testing.T, src *TranscriptSource) []events.Event {
	t.Helper()
	var got []events.Event
	for evt, err := range src.Events(context.Background()) {
		if err != nil {
			t.Fatalf("source error: %v", err)
		}
		got = append(got, evt)
	}
	return got
}

// TestTranscriptSource_EmitsUnredactedEnvelopes guards against a
// regression where TranscriptSource re-acquires its own Redactor.
// Translation is pure; redaction is the consuming Pipeline's job.
// Mirror of the claude/jsonl_test equivalent.
func TestTranscriptSource_EmitsUnredactedEnvelopes(t *testing.T) {
	t.Parallel()
	path := writeJSON(t, userMessageFixture)
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}
	got := collect(t, src)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Envelope.Redaction != nil {
		t.Errorf("Source must emit unredacted envelopes; got Redaction=%+v",
			got[0].Envelope.Redaction)
	}
}

// TestTranscriptSource_OversizeFileIsSkippedNotBuffered verifies a
// session file above the per-source bound is counted as Invalid and
// never read. Without the size guard, os.ReadFile would buffer the
// entire file before json.Unmarshal saw it — a multi-GB session
// (legitimate or corrupted) OOMs the importer.
func TestTranscriptSource_OversizeFileIsSkippedNotBuffered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bigPath := filepath.Join(dir, "big.json")
	// 4 KiB > max=1024 below; we never actually read the bytes.
	body := make([]byte, 4096)
	for i := range body {
		body[i] = 'x'
	}
	if err := os.WriteFile(bigPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(dir, "good.json")
	if err := os.WriteFile(goodPath, []byte(userMessageFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	src := &TranscriptSource{Root: dir, CwdMap: map[string]string{}, maxFileBytes: 1024}
	got := collect(t, src)

	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (oversize file skipped, good file kept)", len(got))
	}
	if src.Stats.Invalid != 1 {
		t.Errorf("Invalid: got %d, want 1", src.Stats.Invalid)
	}
	if src.Stats.FilesRead != 2 {
		t.Errorf("FilesRead: got %d, want 2 (both files are visited; one is rejected pre-read)", src.Stats.FilesRead)
	}
}

func TestTranscriptSource_UserMessage(t *testing.T) {
	t.Parallel()
	path := writeJSON(t, userMessageFixture)
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}

	got := collect(t, src)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	env := got[0].Envelope
	if env.SourceAgent != "gemini-cli" {
		t.Errorf("SourceAgent: got %q", env.SourceAgent)
	}
	if env.Kind != events.KindUserPrompt {
		t.Errorf("Kind: got %q", env.Kind)
	}
	if env.ContentText != "hello gemini" {
		t.Errorf("ContentText: got %q", env.ContentText)
	}
}

func TestTranscriptSource_AssistantWithToolCallFansOut(t *testing.T) {
	t.Parallel()
	path := writeJSON(t, assistantWithToolCallFixture)
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}

	got := collect(t, src)
	// Expect 3 envelopes: assistant_message, tool_use, tool_result
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (msg + use + result)", len(got))
	}
	kinds := []string{got[0].Envelope.Kind, got[1].Envelope.Kind, got[2].Envelope.Kind}
	want := []string{
		events.KindAssistantMessage,
		events.KindToolUse,
		events.KindToolResult,
	}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("kind[%d]: got %q, want %q", i, k, want[i])
		}
	}
}

func TestTranscriptSource_ToolFailureWhenStatusError(t *testing.T) {
	t.Parallel()
	body := `{"sessionId":"s","messages":[{
		"id":"01970000-0000-7000-8000-000000000020",
		"timestamp":"2026-05-02T12:00:00Z",
		"type":"gemini",
		"content":"",
		"toolCalls":[{
			"id":"c1","name":"x","args":{},"status":"error",
			"timestamp":"2026-05-02T12:00:00Z",
			"result":[{"functionResponse":{"id":"c1","name":"x","response":{"output":"fail"}}}]
		}]
	}]}`
	path := writeJSON(t, body)
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}

	got := collect(t, src)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (use + failure)", len(got))
	}
	if got[1].Envelope.Kind != events.KindToolFailure {
		t.Errorf("result kind: got %q, want %q", got[1].Envelope.Kind, events.KindToolFailure)
	}
}

func TestTranscriptSource_StatsCounting(t *testing.T) {
	t.Parallel()
	path := writeJSON(t, userMessageFixture)
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}
	_ = collect(t, src)
	if src.Stats.FilesRead != 1 || src.Stats.MessagesRead != 1 {
		t.Errorf("stats: got %+v", src.Stats)
	}
}

func TestTranscriptSource_MalformedSessionCountedInvalid(t *testing.T) {
	t.Parallel()
	path := writeJSON(t, "not json")
	src := &TranscriptSource{Root: path, CwdMap: map[string]string{}}
	_ = collect(t, src)
	if src.Stats.Invalid != 1 {
		t.Errorf("Invalid count: got %d, want 1", src.Stats.Invalid)
	}
}
