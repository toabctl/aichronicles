package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/agents"
	"github.com/toabctl/aichronicles/pkg/events"
)

// geminiHookKindMap translates Gemini CLI hook_event_name values
// into our canonical Envelope.Kind. Gemini's hook surface is
// documented at https://geminicli.com/docs/hooks/reference; the
// subset here matches what aichronicles subscribes to via
// agents.GeminiCLI.HookEvents.
//
// AfterTool maps to tool_use as a default; AssembleGemini
// promotes it to tool_failure when the response indicates an
// error (gemini reports failures via tool_response.error rather
// than via a dedicated event).
var geminiHookKindMap = map[string]string{
	"BeforeAgent":  events.KindUserPrompt,
	"AfterModel":   events.KindAssistantMessage,
	"AfterTool":    events.KindToolUse,
	"SessionStart": events.KindSessionStart,
	"SessionEnd":   events.KindSessionEnd,
}

// AssembleGemini parses a Gemini CLI hook payload (JSON on stdin)
// and returns a wire Envelope ready to be POSTed. Mirrors the
// shape of Assemble (Claude) and AssembleCodex; the per-host
// quirks live in geminiHookKindMap and extractGeminiContentText.
//
// Gemini's hook input schema is essentially a clone of Claude
// Code's (same {session_id, hook_event_name, cwd, transcript_path,
// timestamp} base) plus per-event extras (tool_name, tool_input,
// tool_response, prompt). Fields we don't map land untouched in
// env.Payload.
func AssembleGemini(raw []byte, now time.Time) (events.Envelope, error) {
	var hook map[string]any
	if err := json.Unmarshal(raw, &hook); err != nil {
		return events.Envelope{}, fmt.Errorf("parse hook payload: %w", err)
	}

	sourceSessionID, _ := hook["session_id"].(string)
	if sourceSessionID == "" {
		return events.Envelope{}, errors.New("hook payload missing session_id")
	}
	hookEvent, _ := hook["hook_event_name"].(string)
	kind, ok := geminiHookKindMap[hookEvent]
	if !ok {
		kind = events.KindUnknown
	}

	// AfterTool with an error response → tool_failure. Detect
	// before building the envelope so role/kind stay consistent
	// (kind=tool_failure → role=tool, same as claude's path).
	if kind == events.KindToolUse && hookEvent == "AfterTool" && geminiToolResponseHasError(hook) {
		kind = events.KindToolFailure
	}

	env := events.Envelope{
		V:               events.CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     agents.GeminiCLI.Slug,
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
	if content := extractGeminiContentText(kind, hookEvent, hook); content != "" {
		env.ContentText = content
	}
	return env, nil
}

// geminiToolResponseHasError reports whether a Gemini AfterTool
// payload carries a non-empty error indicator. Gemini's
// tool_response shape is `{llmContent, returnDisplay, error?, ...}`;
// any non-empty error string (or a non-nil structured error
// object) flips the kind to tool_failure.
//
// Conservative: if we can't determine, default to "no error"
// (i.e., keep kind=tool_use). False negatives here just mean a
// failed tool call lands as tool_use; a noisy false positive
// would inflate the staleness detector's signal.
func geminiToolResponseHasError(hook map[string]any) bool {
	resp, ok := hook["tool_response"].(map[string]any)
	if !ok {
		return false
	}
	switch e := resp["error"].(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(e) != ""
	case bool:
		return e
	case map[string]any:
		// Some hosts emit {error: {message: "...", type: "..."}};
		// any non-empty map counts.
		return len(e) > 0
	}
	return false
}

// extractGeminiContentText pulls the most informative human-
// readable field per kind out of a Gemini hook payload. Empty
// when nothing obvious is there. The mapping mirrors what we
// extract for Claude Code, with two Gemini-specific tweaks:
//
//   - BeforeAgent (Gemini's UserPromptSubmit) carries `prompt`,
//     same as Claude.
//   - AfterModel (Gemini's Stop) carries `response` (the model's
//     text reply) instead of Claude's `last_assistant_message`.
//     Some Gemini versions inline the reply directly under
//     `response`; others wrap it in `{response: {text: ...}}`. We
//     handle both.
func extractGeminiContentText(kind, event string, hook map[string]any) string {
	switch kind {
	case events.KindUserPrompt:
		if s, _ := hook["prompt"].(string); s != "" {
			return s
		}
	case events.KindAssistantMessage:
		// Direct string response.
		if s, _ := hook["response"].(string); s != "" {
			return s
		}
		// Wrapped: {response: {text: "..."}}.
		if m, ok := hook["response"].(map[string]any); ok {
			if s, _ := m["text"].(string); s != "" {
				return s
			}
		}
	case events.KindToolUse, events.KindToolFailure:
		return renderToolContent(hook)
	}
	_ = event
	return ""
}
