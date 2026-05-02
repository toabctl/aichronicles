package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/events"
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

func TestExtractSubagent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hook map[string]any
		want *events.Subagent
	}{
		{
			name: "both id and type → populated",
			hook: map[string]any{"agent_id": "agent-7", "agent_type": "planner"},
			want: &events.Subagent{ID: "agent-7", Type: "planner"},
		},
		{
			name: "id only → populated with empty type",
			hook: map[string]any{"agent_id": "agent-7"},
			want: &events.Subagent{ID: "agent-7", Type: ""},
		},
		{
			// Pinned by the symmetry fix: a payload with only
			// agent_type is malformed — type is descriptive metadata
			// hanging off the id. Returning a Subagent with empty ID
			// would label events for a thread the queries can't
			// reach.
			name: "type only → nil (rejected)",
			hook: map[string]any{"agent_type": "planner"},
			want: nil,
		},
		{
			name: "neither → nil",
			hook: map[string]any{"cwd": "/x"},
			want: nil,
		},
		{
			name: "empty id → nil",
			hook: map[string]any{"agent_id": "", "agent_type": "planner"},
			want: nil,
		},
		{
			name: "non-string id → nil (silent, malformed input)",
			hook: map[string]any{"agent_id": 42, "agent_type": "planner"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractSubagent(tc.hook)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("nil-ness mismatch: got %+v, want %+v", got, tc.want)
			}
			if got == nil {
				return
			}
			if got.ID != tc.want.ID || got.Type != tc.want.Type {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRenderToolContent covers the per-tool tool_input rendering
// that feeds content_text for tool_use / tool_failure events. Each
// case asserts (a) the tool name appears as its own token, and (b)
// the most informative tool_input field is appended so the FTS
// index sees both. Unknown tools fall back to the bare name.
func TestRenderToolContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		hook        map[string]any
		want        string
		wantContain []string // non-empty: substring assertions instead of exact match
	}{
		{
			name: "Bash command in tool_input",
			hook: map[string]any{
				"tool_name":  "Bash",
				"tool_input": map[string]any{"command": "ls -la /tmp"},
			},
			want: "Bash ls -la /tmp",
		},
		{
			name: "Read file_path",
			hook: map[string]any{
				"tool_name":  "Read",
				"tool_input": map[string]any{"file_path": "/etc/hosts"},
			},
			want: "Read /etc/hosts",
		},
		{
			name: "Edit file_path",
			hook: map[string]any{
				"tool_name":  "Edit",
				"tool_input": map[string]any{"file_path": "internal/store/migrate.go"},
			},
			want: "Edit internal/store/migrate.go",
		},
		{
			name: "Grep pattern only",
			hook: map[string]any{
				"tool_name":  "Grep",
				"tool_input": map[string]any{"pattern": "TODO"},
			},
			want: "Grep TODO",
		},
		{
			name: "Grep pattern + path",
			hook: map[string]any{
				"tool_name":  "Grep",
				"tool_input": map[string]any{"pattern": "TODO", "path": "internal/"},
			},
			want: "Grep TODO internal/",
		},
		{
			name: "Glob pattern",
			hook: map[string]any{
				"tool_name":  "Glob",
				"tool_input": map[string]any{"pattern": "**/*.go"},
			},
			want: "Glob **/*.go",
		},
		{
			name: "WebFetch url",
			hook: map[string]any{
				"tool_name":  "WebFetch",
				"tool_input": map[string]any{"url": "https://example.com/x"},
			},
			want: "WebFetch https://example.com/x",
		},
		{
			name: "WebSearch query",
			hook: map[string]any{
				"tool_name":  "WebSearch",
				"tool_input": map[string]any{"query": "Go FTS5 trigram"},
			},
			want: "WebSearch Go FTS5 trigram",
		},
		{
			name: "Task description preferred when present",
			hook: map[string]any{
				"tool_name": "Task",
				"tool_input": map[string]any{
					"description":   "investigate slow query",
					"subagent_type": "planner",
					"prompt":        "Read internal/store/search.go.",
				},
			},
			want: "Task investigate slow query",
		},
		{
			name: "Task falls back to prompt when no description",
			hook: map[string]any{
				"tool_name": "Task",
				"tool_input": map[string]any{
					"prompt": "Read internal/store/search.go.",
				},
			},
			want: "Task Read internal/store/search.go.",
		},
		{
			name: "unknown tool with no string fields falls back to bare name",
			hook: map[string]any{
				"tool_name":  "MysteryTool",
				"tool_input": map[string]any{"flag": true, "count": 3},
			},
			want: "MysteryTool",
		},
		{
			name: "unknown tool surfaces longest string field",
			hook: map[string]any{
				"tool_name": "mcp__demo__doThing",
				"tool_input": map[string]any{
					"flag":  true,
					"short": "x",
					"body":  "the informative payload that should win and is very long",
				},
			},
			want: "mcp__demo__doThing the informative payload that should win and is very long",
		},
		{
			// The B8 audit restraint: short string fields are
			// likelier to be ids/flags/credentials than the payload.
			// Below 16 runes we refuse the fallback and emit just
			// the tool name.
			name: "unknown tool ignores short string fields",
			hook: map[string]any{
				"tool_name": "mcp__demo__doThing",
				"tool_input": map[string]any{
					"value": "tooshort",
				},
			},
			want: "mcp__demo__doThing",
		},
		{
			// The B8 audit restraint: the longest string must
			// clearly dominate runner-up. Two similar-length string
			// fields → no obvious payload → fall back to bare name
			// rather than picking the wrong one.
			name: "unknown tool refuses ambiguous longest-vs-runner-up",
			hook: map[string]any{
				"tool_name": "mcp__demo__doThing",
				"tool_input": map[string]any{
					"hostname": "internal.host.example.com",
					"command":  "do-the-thing-with-secrets",
				},
			},
			want: "mcp__demo__doThing",
		},
		{
			name: "unknown tool ignores nested objects",
			hook: map[string]any{
				"tool_name": "mcp__demo__doThing",
				"tool_input": map[string]any{
					"meta":  map[string]any{"big": "ignored"},
					"value": "the only meaningful string field here",
				},
			},
			want: "mcp__demo__doThing the only meaningful string field here",
		},
		{
			name: "missing tool_input falls back to bare name",
			hook: map[string]any{"tool_name": "Bash"},
			want: "Bash",
		},
		{
			name: "missing tool_name yields empty",
			hook: map[string]any{"tool_input": map[string]any{"command": "x"}},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderToolContent(tc.hook)
			if len(tc.wantContain) == 0 {
				if got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
				return
			}
			for _, s := range tc.wantContain {
				if !strings.Contains(got, s) {
					t.Errorf("got %q, missing substring %q", got, s)
				}
			}
		})
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
	for _, name := range events.ClaudeCode.HookEvents {
		if _, ok := hookKindMap[name]; !ok {
			t.Errorf("installed hook %q has no kind mapping", name)
		}
	}
}
