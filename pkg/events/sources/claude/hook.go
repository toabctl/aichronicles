// Package claude implements events.Source for Claude Code's two
// surfaces: live hook payloads (HookTranslator — single-shot,
// invoked once per stdin payload by `aichronicles ingest`) and
// transcript JSONL files (JSONLSource — streaming, walks
// ~/.claude/projects/*.jsonl).
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/agents"
	"github.com/toabctl/aichronicles/pkg/events"
)

// hookKindMap translates Claude Code hook_event_name values to our
// canonical Envelope.Kind. Anything not listed maps to KindUnknown
// so new hook events surface through observability rather than
// crashing.
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

// HookTranslator parses a Claude Code hook payload (JSON on stdin)
// into a canonical Envelope, applies redaction, and returns it
// ready to POST. Single-shot — the `aichronicles ingest`
// subprocess uses one HookTranslator per hook firing. For
// streaming over a transcript file, see JSONLSource.
//
// Holds a Redactor so redaction is enforced as part of translation
// (sources are the edge — the Pipeline's RequireRedaction gate is
// defense-in-depth, not the primary enforcement). Now is injectable
// for tests; nil falls back to time.Now (UTC).
type HookTranslator struct {
	Redactor events.Redactor
	Now      func() time.Time
}

// Translate parses raw and returns a fully-populated Envelope with
// redaction applied. The returned Envelope's TsSource is captured
// from t.Now() (or time.Now().UTC() when nil) at translation time.
func (t *HookTranslator) Translate(raw []byte) (events.Envelope, error) {
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

	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now()
	}

	env := events.Envelope{
		V:               events.CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     agents.ClaudeCode.Slug,
		SourceSessionID: sourceSessionID,
		Kind:            kind,
		Role:            events.RoleForKind(kind),
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

	if t.Redactor != nil {
		t.Redactor.Apply(&env)
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

// extractContentText pulls the most informative human-readable
// field from the hook payload per kind. Empty when nothing obvious
// is there. Field names reflect what Claude Code's hook runtime
// actually sends.
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
		return events.RenderToolContent(hook)
	}
	return ""
}
