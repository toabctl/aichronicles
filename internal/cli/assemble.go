// Package cli holds the cobra commands and supporting logic for the
// aichronicles binary — chiefly the `ingest` subcommand invoked by
// Claude Code hooks.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// hookKindMap translates Claude Code hook_event_name values to our
// canonical Envelope.Kind. Anything not listed maps to "unknown" so new
// hook events surface through observability rather than crashing.
var hookKindMap = map[string]string{
	"UserPromptSubmit":    "user_prompt",
	"UserPromptExpansion": "user_prompt",
	"Stop":                "assistant_message",
	"SessionStart":        "session_start",
	"SessionEnd":          "session_end",
	"PostToolUse":         "tool_use",
	"PostToolUseFailure":  "tool_failure",
	"PostToolBatch":       "tool_use",
	"SubagentStart":       "subagent_start",
	"SubagentStop":        "subagent_stop",
	"PreCompact":          "compact_start",
	"PostCompact":         "compact_end",
	"CwdChanged":          "cwd_changed",
	"InstructionsLoaded":  "instructions_loaded",
	"Notification":        "system_message",
}

// roleForKind fills in the Envelope.Role hint from the canonical kind.
// It is a pure convenience: downstream queries can always check kind
// directly; role exists so cross-source queries can filter on it.
func roleForKind(kind string) string {
	switch kind {
	case "user_prompt":
		return "user"
	case "assistant_message":
		return "assistant"
	case "tool_use", "tool_result", "tool_failure":
		return "tool"
	case "session_start", "session_end",
		"compact_start", "compact_end",
		"subagent_start", "subagent_stop",
		"cwd_changed", "instructions_loaded",
		"system_message", "error":
		return "system"
	default:
		return ""
	}
}

// AssembleByAgent dispatches to the right per-agent assembler. The
// agent slug must match one of the known sources (today: claude-code,
// codex). Unknown slugs return an error so a typo in --agent surfaces
// immediately rather than producing a malformed envelope.
func AssembleByAgent(agent string, raw []byte, now time.Time) (ingest.Envelope, error) {
	switch agent {
	case "claude-code":
		return Assemble(raw, now)
	case "codex":
		return AssembleCodex(raw, now)
	default:
		return ingest.Envelope{}, fmt.Errorf("AssembleByAgent: unknown agent slug %q", agent)
	}
}

// Assemble parses a Claude Code hook payload (JSON on stdin) and returns
// a wire Envelope ready to be POSTed. The payload is stored verbatim so
// downstream enrichment can recover anything we didn't normalize.
func Assemble(raw []byte, now time.Time) (ingest.Envelope, error) {
	var hook map[string]any
	if err := json.Unmarshal(raw, &hook); err != nil {
		return ingest.Envelope{}, fmt.Errorf("parse hook payload: %w", err)
	}

	sourceSessionID, _ := hook["session_id"].(string)
	if sourceSessionID == "" {
		return ingest.Envelope{}, errors.New("hook payload missing session_id")
	}
	hookEvent, _ := hook["hook_event_name"].(string)
	kind, ok := hookKindMap[hookEvent]
	if !ok {
		kind = "unknown"
	}

	env := ingest.Envelope{
		V:               ingest.CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     ingest.ClaudeCode.Slug,
		SourceSessionID: sourceSessionID,
		Kind:            kind,
		Role:            roleForKind(kind),
		TsSource:        now,
		Transport:       "hook",
		Payload:         hook,
	}
	if cwd, ok := hook["cwd"].(string); ok && cwd != "" {
		env.Cwd = cwd
	}
	if toolName, ok := hook["tool_name"].(string); ok && toolName != "" {
		env.Tool = &ingest.Tool{Name: toolName, NameRaw: toolName}
	}
	if content := extractContentText(kind, hook); content != "" {
		env.ContentText = content
	}
	if sa := extractSubagent(hook); sa != nil {
		env.Subagent = sa
	}
	return env, nil
}

// extractSubagent pulls subagent identity from the hook payload.
// Claude Code emits agent_id / agent_type on SubagentStart,
// SubagentStop, and any tool_use that fires inside a subagent's
// frame. Returns nil for top-level events so the envelope omits
// the field on the wire entirely.
func extractSubagent(hook map[string]any) *ingest.Subagent {
	id, _ := hook["agent_id"].(string)
	typ, _ := hook["agent_type"].(string)
	if id == "" && typ == "" {
		return nil
	}
	return &ingest.Subagent{ID: id, Type: typ}
}

// extractContentText pulls the most informative human-readable field
// from the hook payload per kind. Empty when nothing obvious is there.
// Field names reflect what Claude Code's hook runtime actually sends
// (see internal/cli/testdata/hooks/*.json for samples).
func extractContentText(kind string, hook map[string]any) string {
	switch kind {
	case "user_prompt":
		if s, ok := hook["prompt"].(string); ok {
			return s
		}
	case "assistant_message":
		// Stop hooks carry the full assistant turn text here.
		if s, ok := hook["last_assistant_message"].(string); ok {
			return s
		}
	case "tool_use", "tool_failure":
		return renderToolContent(hook)
	}
	return ""
}

// renderToolContent produces a one-liner suitable for content_text
// and FTS indexing, derived from a tool_use payload's tool_name and
// tool_input. The tool name leads as its own token so queries for
// the tool itself still match; the most informative tool_input
// field follows so a search like `cluster` finds Bash invocations
// whose command mentions cluster, or Grep invocations whose pattern
// contains cluster, without depending on the extractions fallback.
//
// Falls back to the bare tool name (or empty) for tools whose
// tool_input we don't know how to render — preserves the
// pre-existing behaviour for everything we haven't enumerated.
func renderToolContent(hook map[string]any) string {
	name, _ := hook["tool_name"].(string)
	if name == "" {
		return ""
	}
	input, _ := hook["tool_input"].(map[string]any)
	detail := toolDetail(name, input)
	if detail == "" {
		return name
	}
	return name + " " + detail
}

// toolDetail picks the most informative single string from a known
// tool's tool_input. Returns empty for unknown tools, which makes
// renderToolContent fall back to the bare tool name. Adding a new
// tool here should be paired with a matching extractor in
// pkg/ingest/extract so the typed-fact tier can also reach it.
func toolDetail(toolName string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch toolName {
	case "Bash":
		return stringField(input, "command")
	case "Read", "Write", "Edit", "NotebookEdit":
		return stringField(input, "file_path")
	case "Grep":
		pat := stringField(input, "pattern")
		path := stringField(input, "path")
		if pat != "" && path != "" {
			return pat + " " + path
		}
		return pat
	case "Glob":
		return stringField(input, "pattern")
	case "WebFetch":
		return stringField(input, "url")
	case "WebSearch":
		return stringField(input, "query")
	case "Task":
		// Sub-agent launch. Both fields are typically present;
		// description is shorter and clearer for a one-line
		// preview, prompt is the full instructions.
		if d := stringField(input, "description"); d != "" {
			return d
		}
		return stringField(input, "prompt")
	}
	// Unknown tool (commonly an MCP tool — `mcp__server__name`
	// in Claude Code's namespace, with a per-server tool_input
	// schema we can't enumerate). Fall back to the longest
	// string-valued field in tool_input so the most informative
	// payload still reaches FTS without a per-server allow-list.
	return longestStringValue(input)
}

// stringField returns m[key] as a string when present and non-empty,
// "" otherwise. Avoids the awkward two-step ok-check at every call
// site without hiding the type assertion.
func stringField(m map[string]any, key string) string {
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}

// longestStringValue returns the longest string-typed value in m, or
// "" when no top-level string field is present. Used as the
// fallback rendering for tools we don't know per-shape — MCP tools
// typically carry one big string field (a query, a path, a body)
// alongside knobs; surfacing the longest one is a cheap proxy for
// "the informative part." Non-string fields (numbers, nested
// objects) are ignored deliberately to keep content_text tight.
func longestStringValue(m map[string]any) string {
	if m == nil {
		return ""
	}
	var best string
	for _, v := range m {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if len(s) > len(best) {
			best = s
		}
	}
	return best
}
