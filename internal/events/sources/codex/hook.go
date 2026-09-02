// Package codex implements events.Source for OpenAI's Codex CLI:
// hook payloads (HookTranslator — single-shot, invoked once per
// stdin payload by `aichronicles hook --agent codex-cli`).
//
// Codex has no transcript importer yet. It writes rollout JSONL
// under $CODEX_HOME/sessions/<yyyy>/<mm>/<dd>/rollout-*.jsonl in
// the OpenAI Responses item shape, which is a different animal
// from the two transcript formats we already walk; backfilling it
// is deliberately left out rather than guessed at.
package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/agents"
	"github.com/toabctl/aichronicles/internal/events"
)

// hookKindMap translates Codex CLI hook_event_name values into our
// canonical Envelope.Kind. Codex reuses Claude Code's PascalCase
// event names verbatim, so this reads like the claude map — with
// one deliberate omission: there is no PostToolUseFailure, and no
// other event that means "a tool failed" (see toolFailure note on
// Translate).
//
// The map is wider than agents.CodexCLI.HookEvents on purpose: a
// user who hand-registers `aichronicles hook --agent codex-cli`
// against PreCompact or SubagentStop gets a correctly-typed
// envelope instead of KindUnknown. Anything genuinely unrecognised
// still lands as KindUnknown so new Codex events surface through
// observability rather than crash.
var hookKindMap = map[string]string{
	"UserPromptSubmit": events.KindUserPrompt,
	"Stop":             events.KindAssistantMessage,
	"SessionStart":     events.KindSessionStart,
	"SessionEnd":       events.KindSessionEnd,
	"PostToolUse":      events.KindToolUse,
	"SubagentStart":    events.KindSubagentStart,
	"SubagentStop":     events.KindSubagentStop,
	"PreCompact":       events.KindCompactStart,
	"PostCompact":      events.KindCompactEnd,
}

// HookTranslator parses a Codex CLI hook payload (JSON on stdin)
// into a canonical Envelope ready to POST. Mirrors the Claude and
// Gemini translators' contract: single-shot, and pure — redaction
// is applied server-side by the receiving daemon's events.Pipeline,
// which is the single point of enforcement for the "no unredacted
// secrets on disk" invariant. Now is injectable for tests; nil
// falls back to time.Now (UTC).
type HookTranslator struct {
	Now func() time.Time
}

// Translate parses raw and returns a fully-populated Envelope.
//
// Every PostToolUse maps to tool_use, never tool_failure. Codex's
// PostToolUse carries tool_response as the raw tool output — for a
// shell call, literally the process's combined output with no exit
// code, no error field and no status. `cat missing.txt` yields
// "cat: missing.txt: No such file or directory\n", which is a
// perfectly ordinary output string; the only way to call that a
// failure would be to pattern-match prose. Recording a wrong
// tool_failure is worse than recording no failure at all, so we
// record the fact (a tool ran, with this input and this output)
// and leave the judgement to a later layer.
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
		SourceAgent:     agents.CodexCLI.Slug,
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

	return env, nil
}

// extractSubagent pulls subagent identity from the hook payload.
// Codex's post-tool-use / subagent-* inputs carry optional
// agent_id and agent_type, same names as Claude Code's.
//
// Returns nil unless agent_id is present and non-empty: a payload
// with only agent_type would produce a thread labelled with an
// empty ID, which downstream queries cannot reason about.
func extractSubagent(hook map[string]any) *events.Subagent {
	id, _ := hook["agent_id"].(string)
	if id == "" {
		return nil
	}
	typ, _ := hook["agent_type"].(string)
	return &events.Subagent{ID: id, Type: typ}
}

// extractContentText pulls the most informative human-readable
// field per kind out of a Codex hook payload. The field names are
// Claude Code's — `prompt` on UserPromptSubmit,
// `last_assistant_message` on Stop — and so is the tool payload
// shape, so tool events go through the shared renderer unchanged
// (Codex reports a shell call as tool_name="Bash" with
// tool_input={"command": ...}).
func extractContentText(kind string, hook map[string]any) string {
	switch kind {
	case events.KindUserPrompt:
		if s, _ := hook["prompt"].(string); s != "" {
			return s
		}
	case events.KindAssistantMessage:
		if s, _ := hook["last_assistant_message"].(string); s != "" {
			return s
		}
	case events.KindToolUse, events.KindToolFailure:
		return events.RenderToolContent(hook)
	}
	return ""
}
