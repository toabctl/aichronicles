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

	"github.com/toabctl/aichronicles/pkg/events"
)

// hookKindMap translates Claude Code hook_event_name values to our
// canonical Envelope.Kind. Anything not listed maps to events.KindUnknown
// so new hook events surface through observability rather than crashing.
var hookKindMap = map[string]string{
	"UserPromptSubmit":    events.KindUserPrompt,
	"UserPromptExpansion": events.KindUserPrompt,
	"Stop":                events.KindAssistantMessage,
	"SessionStart":        events.KindSessionStart,
	"SessionEnd":          events.KindSessionEnd,
	"PostToolUse":         events.KindToolUse,
	"PostToolUseFailure":  events.KindToolFailure,
	"PostToolBatch":       events.KindToolUse,
	"SubagentStart":       events.KindSubagentStart,
	"SubagentStop":        events.KindSubagentStop,
	"PreCompact":          events.KindCompactStart,
	"PostCompact":         events.KindCompactEnd,
	"CwdChanged":          events.KindCwdChanged,
	"InstructionsLoaded":  events.KindInstructionsLoaded,
	"Notification":        events.KindSystemMessage,
}

// roleForKind fills in the Envelope.Role hint from the canonical kind.
// It is a pure convenience: downstream queries can always check kind
// directly; role exists so cross-source queries can filter on it.
func roleForKind(kind string) string {
	switch kind {
	case events.KindUserPrompt:
		return events.RoleUser
	case events.KindAssistantMessage:
		return events.RoleAssistant
	case events.KindToolUse, events.KindToolResult, events.KindToolFailure:
		return events.RoleTool
	case events.KindSessionStart, events.KindSessionEnd,
		events.KindCompactStart, events.KindCompactEnd,
		events.KindSubagentStart, events.KindSubagentStop,
		events.KindCwdChanged, events.KindInstructionsLoaded,
		events.KindSystemMessage, events.KindError:
		return events.RoleSystem
	default:
		return ""
	}
}

// AssembleByAgent dispatches to the right per-agent assembler. The
// agent slug must match one of the known sources (claude-code,
// gemini-cli). Unknown slugs return an error so a typo in --agent
// surfaces immediately rather than producing a malformed envelope.
func AssembleByAgent(agent string, raw []byte, now time.Time) (events.Envelope, error) {
	switch agent {
	case "claude-code":
		return Assemble(raw, now)
	case "gemini-cli":
		return AssembleGemini(raw, now)
	default:
		return events.Envelope{}, fmt.Errorf("AssembleByAgent: unknown agent slug %q", agent)
	}
}

// Assemble parses a Claude Code hook payload (JSON on stdin) and returns
// a wire Envelope ready to be POSTed. The payload is stored verbatim so
// downstream enrichment can recover anything we didn't normalize.
func Assemble(raw []byte, now time.Time) (events.Envelope, error) {
	var hook map[string]any
	if err := json.Unmarshal(raw, &hook); err != nil {
		return events.Envelope{}, fmt.Errorf("parse hook payload: %w", err)
	}

	sourceSessionID, _ := hook["session_id"].(string)
	if sourceSessionID == "" {
		return events.Envelope{}, errors.New("hook payload missing session_id")
	}
	hookEvent, _ := hook["hook_event_name"].(string)
	kind, ok := hookKindMap[hookEvent]
	if !ok {
		kind = events.KindUnknown
	}

	env := events.Envelope{
		V:               events.CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     events.ClaudeCode.Slug,
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
		env.Tool = &events.Tool{Name: toolName}
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
// frame.
//
// Returns nil unless agent_id is present and non-empty: subagent
// identity hangs off the ID — type is descriptive metadata. A
// payload with only agent_type and no agent_id is malformed (or
// from a host we don't understand); fabricating a thread out of
// it would label events with an empty ID, which downstream
// queries can't reason about.
func extractSubagent(hook map[string]any) *events.Subagent {
	id, _ := hook["agent_id"].(string)
	if id == "" {
		return nil
	}
	typ, _ := hook["agent_type"].(string)
	return &events.Subagent{ID: id, Type: typ}
}

// extractContentText pulls the most informative human-readable field
// from the hook payload per kind. Empty when nothing obvious is there.
// Field names reflect what Claude Code's hook runtime actually sends
// (see internal/cli/testdata/hooks/*.json for samples).
func extractContentText(kind string, hook map[string]any) string {
	switch kind {
	case events.KindUserPrompt:
		if s, ok := hook["prompt"].(string); ok {
			return s
		}
	case events.KindAssistantMessage:
		// Stop hooks carry the full assistant turn text here.
		if s, ok := hook["last_assistant_message"].(string); ok {
			return s
		}
	case events.KindToolUse, events.KindToolFailure:
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
// pkg/events/extract so the typed-fact tier can also reach it.
//
// Both Claude Code's tool naming (PascalCase: Bash, Read, …) and
// Gemini CLI's equivalents (snake_case: run_shell_command,
// read_file, …) are handled here. The tool_input field names are
// identical across the two agents — both pass `{command, ...}`
// for shell, `{file_path, ...}` for file ops, etc. — so one
// switch with both names per case keeps the renderer consistent.
func toolDetail(toolName string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch toolName {
	case "Bash", "run_shell_command":
		return stringField(input, "command")
	case "Read", "read_file",
		"Write", "write_file",
		"Edit", "replace",
		"NotebookEdit":
		// Gemini's read_file uses `absolute_path`; write_file and
		// replace use `file_path`. Both shapes covered here.
		if p := stringField(input, "file_path"); p != "" {
			return p
		}
		return stringField(input, "absolute_path")
	case "Grep", "search_file_content":
		pat := stringField(input, "pattern")
		path := stringField(input, "path")
		if pat != "" && path != "" {
			return pat + " " + path
		}
		return pat
	case "Glob", "find":
		return stringField(input, "pattern")
	case "WebFetch", "web_fetch":
		return stringField(input, "url")
	case "WebSearch", "google_web_search":
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

// longestStringValue returns the longest string-typed value in m
// when it's clearly the informative payload, or "" otherwise. Used
// as the fallback rendering for tools we don't know per-shape —
// MCP tools typically carry one big string field (a query, a path,
// a body) alongside knobs; surfacing the longest one is a cheap
// proxy for "the informative part."
//
// Two restraints from the B8 audit fix:
//
//   - Minimum length of fallbackMinLen runes before we'll surface a
//     value at all. A short string field is more likely a flag,
//     id, or credential than a payload — we'd rather render the
//     bare tool name than risk a false-positive secret leak.
//
//   - The longest value must clearly dominate any other strings.
//     If two fields are similar lengths, neither is "obviously the
//     payload"; refuse and fall back to bare tool name. Mitigates
//     the risk of picking the wrong field when the schema gives
//     no guidance.
//
// Non-string fields (numbers, nested objects, booleans) are ignored
// deliberately to keep content_text tight.
const (
	fallbackMinLen      = 16 // shorter than this is likely a knob, not the payload
	fallbackDominanceX2 = 2  // longest must be at least 2× the runner-up to win
)

func longestStringValue(m map[string]any) string {
	if m == nil {
		return ""
	}
	var best, secondBest string
	for _, v := range m {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if len(s) > len(best) {
			secondBest = best
			best = s
		} else if len(s) > len(secondBest) {
			secondBest = s
		}
	}
	if len([]rune(best)) < fallbackMinLen {
		return ""
	}
	// "Clearly dominates" check: longest at least 2× the runner-up,
	// or there's no runner-up at all (only one string field).
	if secondBest != "" && len(best) < fallbackDominanceX2*len(secondBest) {
		return ""
	}
	return best
}
