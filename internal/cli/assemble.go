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

// claudeCodeAgent is the fixed source_agent slug for Claude Code hooks.
const claudeCodeAgent = "claude-code"

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
		SourceAgent:     claudeCodeAgent,
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
	return env, nil
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
		// A best-effort rendering of tool invocations for FTS.
		if toolName, ok := hook["tool_name"].(string); ok {
			return toolName
		}
	}
	return ""
}
