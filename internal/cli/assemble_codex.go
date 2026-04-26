package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// codexHookKindMap translates Codex CLI hook_event_name values to
// our canonical Envelope.Kind. The set is narrower than Claude
// Code's because Codex publishes fewer hook events (per
// https://developers.openai.com/codex/hooks): no SessionStart /
// SessionEnd, no SubagentStart / SubagentStop, no PreCompact.
//
// The implementation is based on the documented hook payload
// shapes; once we have a real Codex fixture we may need to tweak
// individual field names. Pin this table with golden tests as
// soon as fixtures land.
var codexHookKindMap = map[string]string{
	"UserPromptSubmit":   "user_prompt",
	"Stop":               "assistant_message",
	"PostToolUse":        "tool_use",
	"PostToolUseFailure": "tool_failure",
}

// AssembleCodex parses a Codex CLI hook payload (JSON on stdin)
// and returns a wire Envelope ready to be POSTed. Mirrors the
// shape of Assemble but maps Codex-specific event names and
// fields to the canonical envelope. The full hook payload is
// preserved in env.Payload so downstream enrichment can recover
// fields we don't normalise here.
//
// Codex's documented payload shape is similar to Claude Code's
// (session_id, hook_event_name, cwd, tool_name, tool_input,
// prompt for UserPromptSubmit, etc.); fields we don't map land
// untouched in env.Payload.
func AssembleCodex(raw []byte, now time.Time) (ingest.Envelope, error) {
	var hook map[string]any
	if err := json.Unmarshal(raw, &hook); err != nil {
		return ingest.Envelope{}, fmt.Errorf("parse hook payload: %w", err)
	}

	sourceSessionID, _ := hook["session_id"].(string)
	if sourceSessionID == "" {
		return ingest.Envelope{}, errors.New("hook payload missing session_id")
	}
	hookEvent, _ := hook["hook_event_name"].(string)
	kind, ok := codexHookKindMap[hookEvent]
	if !ok {
		kind = "unknown"
	}

	env := ingest.Envelope{
		V:               ingest.CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     ingest.Codex.Slug,
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
	if content := extractCodexContentText(kind, hook); content != "" {
		env.ContentText = content
	}
	return env, nil
}

// extractCodexContentText pulls the human-readable content per
// kind from a Codex hook payload. The mapping is documented-
// shape based: Codex's UserPromptSubmit carries `prompt`,
// PostToolUse carries `tool_name` (and we delegate to the same
// per-tool renderer Claude Code uses since tool_input shape is
// largely tool-driven not host-driven).
func extractCodexContentText(kind string, hook map[string]any) string {
	switch kind {
	case "user_prompt":
		if s, ok := hook["prompt"].(string); ok {
			return s
		}
	case "assistant_message":
		// Codex's Stop hook carries the assistant turn under a
		// field whose exact name needs fixture confirmation;
		// try the same key Claude Code uses, then fall back to
		// "response".
		if s, ok := hook["last_assistant_message"].(string); ok && s != "" {
			return s
		}
		if s, ok := hook["response"].(string); ok && s != "" {
			return s
		}
	case "tool_use", "tool_failure":
		return renderToolContent(hook)
	}
	return ""
}
