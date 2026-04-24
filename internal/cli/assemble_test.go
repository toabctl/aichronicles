package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixture describes the expected shape of an assembled envelope for a
// single captured hook payload in internal/cli/testdata/hooks/.
type fixture struct {
	file                string
	wantKind            string
	wantRole            string
	wantCwd             string
	wantTool            string // expected Tool.Name (empty = Tool should be nil)
	contentTextContains string // substring; empty = content text expected empty
	sourceSessionIDHead string // first 8 chars of the expected source_session_id
}

// fixtures are real captured payloads from a live Claude Code session,
// anonymized and committed under testdata/hooks. See README there for
// provenance and redaction rules.
var fixtures = []fixture{
	{
		file:                "session_start.json",
		wantKind:            "session_start",
		wantRole:            "system",
		wantCwd:             "/home/user/project",
		sourceSessionIDHead: "dd445560",
	},
	{
		file:                "user_prompt.json",
		wantKind:            "user_prompt",
		wantRole:            "user",
		wantCwd:             "/home/user/project",
		contentTextContains: "what is jsonl",
		sourceSessionIDHead: "1115d109",
	},
	{
		file:                "assistant_message.json",
		wantKind:            "assistant_message",
		wantRole:            "assistant",
		wantCwd:             "/home/user/project",
		contentTextContains: "JSON Lines",
		sourceSessionIDHead: "1115d109",
	},
	{
		file:                "tool_use_bash.json",
		wantKind:            "tool_use",
		wantRole:            "tool",
		wantCwd:             "/home/user/project",
		wantTool:            "Bash",
		contentTextContains: "Bash",
		sourceSessionIDHead: "1115d109",
	},
	{
		file:                "tool_use_read.json",
		wantKind:            "tool_use",
		wantRole:            "tool",
		wantCwd:             "/home/user/project",
		wantTool:            "Read",
		contentTextContains: "Read",
		sourceSessionIDHead: "1115d109",
	},
	{
		file:                "tool_failure.json",
		wantKind:            "tool_failure",
		wantRole:            "tool",
		wantCwd:             "/home/user/project",
		wantTool:            "Bash",
		contentTextContains: "Bash",
		sourceSessionIDHead: "1115d109",
	},
	{
		file:                "session_end.json",
		wantKind:            "session_end",
		wantRole:            "system",
		wantCwd:             "/home/user/project",
		sourceSessionIDHead: "886d5e67",
	},
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "hooks", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return raw
}

func TestAssemble_RealHookFixtures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	for _, fx := range fixtures {
		t.Run(fx.file, func(t *testing.T) {
			t.Parallel()
			raw := loadFixture(t, fx.file)

			env, err := Assemble(raw, now)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}

			if env.V != 1 {
				t.Errorf("V: got %d, want 1", env.V)
			}
			if _, err := uuid.Parse(env.EventID); err != nil {
				t.Errorf("EventID not a UUID: %v", err)
			}
			if env.SourceAgent != "claude-code" {
				t.Errorf("SourceAgent: got %q", env.SourceAgent)
			}
			if !strings.HasPrefix(env.SourceSessionID, fx.sourceSessionIDHead) {
				t.Errorf("SourceSessionID: got %q, want prefix %q", env.SourceSessionID, fx.sourceSessionIDHead)
			}
			if env.Kind != fx.wantKind {
				t.Errorf("Kind: got %q, want %q", env.Kind, fx.wantKind)
			}
			if env.Role != fx.wantRole {
				t.Errorf("Role: got %q, want %q", env.Role, fx.wantRole)
			}
			if env.Cwd != fx.wantCwd {
				t.Errorf("Cwd: got %q, want %q", env.Cwd, fx.wantCwd)
			}
			if env.Transport != "hook" {
				t.Errorf("Transport: got %q", env.Transport)
			}
			if !env.TsSource.Equal(now) {
				t.Errorf("TsSource: got %v, want %v", env.TsSource, now)
			}

			if fx.wantTool == "" {
				if env.Tool != nil {
					t.Errorf("Tool: got %+v, want nil", env.Tool)
				}
			} else {
				if env.Tool == nil {
					t.Fatalf("Tool: got nil, want Name=%q", fx.wantTool)
				}
				if env.Tool.Name != fx.wantTool {
					t.Errorf("Tool.Name: got %q, want %q", env.Tool.Name, fx.wantTool)
				}
			}

			if fx.contentTextContains == "" {
				if env.ContentText != "" {
					t.Errorf("ContentText: got %q, want empty", env.ContentText)
				}
			} else if !strings.Contains(env.ContentText, fx.contentTextContains) {
				t.Errorf("ContentText: got %q, want substring %q", env.ContentText, fx.contentTextContains)
			}

			// Validate() should succeed for every real envelope we assemble.
			if err := env.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}

			// Payload must round-trip verbatim so enrichment can recover
			// fields we chose not to promote to envelope columns.
			if len(env.Payload) == 0 {
				t.Errorf("Payload empty — should preserve full hook payload")
			}
		})
	}
}

// Edge cases that don't need a fixture — small synthetic inputs make
// the intent obvious.

func TestAssemble_UnknownHookEventMapsToUnknown(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"session_id":"s","hook_event_name":"NotYetSupported","cwd":"/"}`)
	env, err := Assemble(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if env.Kind != "unknown" {
		t.Errorf("Kind: got %q, want unknown", env.Kind)
	}
	if env.Role != "" {
		t.Errorf("Role should be empty for unknown kind, got %q", env.Role)
	}
	if env.Payload["hook_event_name"] != "NotYetSupported" {
		t.Errorf("raw hook name lost from payload")
	}
}

func TestAssemble_MissingSessionID_IsError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"UserPromptSubmit","cwd":"/"}`)
	_, err := Assemble(raw, time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for missing session_id")
	}
}

func TestAssemble_MalformedJSON_IsError(t *testing.T) {
	t.Parallel()
	_, err := Assemble([]byte("{not json"), time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestRoleForKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"user_prompt":         "user",
		"assistant_message":   "assistant",
		"tool_use":            "tool",
		"tool_result":         "tool",
		"tool_failure":        "tool",
		"session_start":       "system",
		"compact_end":         "system",
		"cwd_changed":         "system",
		"instructions_loaded": "system",
		"unknown":             "",
	}
	for kind, wantRole := range cases {
		if got := roleForKind(kind); got != wantRole {
			t.Errorf("roleForKind(%q) = %q, want %q", kind, got, wantRole)
		}
	}
}

func TestHookKindMap_CoversInstalledHooks(t *testing.T) {
	t.Parallel()
	for _, name := range installedHooks {
		if _, ok := hookKindMap[name]; !ok {
			t.Errorf("installed hook %q has no kind mapping", name)
		}
	}
}
