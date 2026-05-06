// Package gemini implements events.Source for Google's Gemini CLI:
// hook payloads (HookTranslator — single-shot) and on-disk session
// files (TranscriptSource — streaming).
package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/agents"
	"github.com/toabctl/aichronicles/internal/events"
)

// hookKindMap translates Gemini CLI hook_event_name values into our
// canonical Envelope.Kind. Documented at
// https://geminicli.com/docs/hooks/reference; the subset here matches
// what aichronicles subscribes to via agents.GeminiCLI.HookEvents.
var hookKindMap = map[string]string{
	"BeforeAgent":  events.KindUserPrompt,
	"AfterModel":   events.KindAssistantMessage,
	"AfterTool":    events.KindToolUse,
	"SessionStart": events.KindSessionStart,
	"SessionEnd":   events.KindSessionEnd,
}

// HookTranslator parses a Gemini CLI hook payload (JSON on stdin)
// into a canonical Envelope and returns it ready to POST. Mirrors
// the Claude HookTranslator's contract; the Gemini-specific quirks
// live in hookKindMap and extractContentText. Translation is pure;
// redaction is performed server-side by the daemon's
// events.Pipeline.
type HookTranslator struct {
	Now func() time.Time
}

// Translate parses raw and returns a fully-populated Envelope.
// AfterTool is promoted to tool_failure when the tool_response
// carries an error indicator (Gemini reports failures via
// tool_response.error rather than via a dedicated event).
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
	if kind == events.KindToolUse && hookEvent == "AfterTool" && toolResponseHasError(hook) {
		kind = events.KindToolFailure
	}

	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now()
	}

	env := events.Envelope{
		V:               events.CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     agents.GeminiCLI.Slug,
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

	return env, nil
}

// toolResponseHasError reports whether a Gemini AfterTool payload
// carries a non-empty error indicator. Gemini's tool_response shape
// is `{llmContent, returnDisplay, error?, ...}`; any non-empty
// error string (or non-nil structured error) flips kind to
// tool_failure. Conservative: when in doubt, default to "no error"
// (keep kind=tool_use).
func toolResponseHasError(hook map[string]any) bool {
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
		return len(e) > 0
	}
	return false
}

// extractContentText pulls the most informative human-readable
// field per kind out of a Gemini hook payload. Mirrors what we
// extract for Claude with two Gemini-specific tweaks:
//
//   - BeforeAgent (Gemini's UserPromptSubmit) carries `prompt`,
//     same as Claude.
//   - AfterModel (Gemini's Stop) carries `response` (model text)
//     instead of Claude's `last_assistant_message`. Some versions
//     inline the reply directly under `response`; others wrap it
//     in `{response: {text: ...}}`. Both shapes covered.
func extractContentText(kind string, hook map[string]any) string {
	switch kind {
	case events.KindUserPrompt:
		if s, _ := hook["prompt"].(string); s != "" {
			return s
		}
	case events.KindAssistantMessage:
		if s, _ := hook["response"].(string); s != "" {
			return s
		}
		if m, ok := hook["response"].(map[string]any); ok {
			if s, _ := m["text"].(string); s != "" {
				return s
			}
		}
	case events.KindToolUse, events.KindToolFailure:
		return events.RenderToolContent(hook)
	}
	return ""
}
