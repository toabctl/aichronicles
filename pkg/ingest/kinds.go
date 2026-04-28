package ingest

// Canonical event kinds — the closed vocabulary stored in
// events.kind. Source of truth for the wire schema; api/openapi.yaml
// mirrors the same set. Adding a kind requires updating both this
// file (so Validate accepts it) and the OpenAPI enum.
//
// String constants rather than a typed alias to keep call sites
// terse: SQL bind args, fmt.Sprintf, and map keys all consume them
// directly. The same pattern extract.Kind* uses for extraction
// values.
const (
	KindUserPrompt         = "user_prompt"
	KindAssistantMessage   = "assistant_message"
	KindToolUse            = "tool_use"
	KindToolResult         = "tool_result"
	KindToolFailure        = "tool_failure"
	KindSessionStart       = "session_start"
	KindSessionEnd         = "session_end"
	KindSubagentStart      = "subagent_start"
	KindSubagentStop       = "subagent_stop"
	KindCompactStart       = "compact_start"
	KindCompactEnd         = "compact_end"
	KindCwdChanged         = "cwd_changed"
	KindInstructionsLoaded = "instructions_loaded"
	KindSystemMessage      = "system_message"
	KindError              = "error"
	KindUnknown            = "unknown"
)

// validKinds is the lookup set IsValidKind consults. Built once at
// package init from the constants above so a new kind only has to be
// added in one place. Map-of-empty-struct rather than a slice scan
// because callers (Envelope.Validate, future MCP tools) hit it on
// every event.
var validKinds = map[string]struct{}{
	KindUserPrompt:         {},
	KindAssistantMessage:   {},
	KindToolUse:            {},
	KindToolResult:         {},
	KindToolFailure:        {},
	KindSessionStart:       {},
	KindSessionEnd:         {},
	KindSubagentStart:      {},
	KindSubagentStop:       {},
	KindCompactStart:       {},
	KindCompactEnd:         {},
	KindCwdChanged:         {},
	KindInstructionsLoaded: {},
	KindSystemMessage:      {},
	KindError:              {},
	KindUnknown:            {},
}

// IsValidKind reports whether s is one of the canonical event kinds.
// Empty string returns false.
func IsValidKind(s string) bool {
	_, ok := validKinds[s]
	return ok
}

// Canonical role values — the closed vocabulary stored in
// events.role. Mirrors the OpenAPI role enum.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

var validRoles = map[string]struct{}{
	RoleUser:      {},
	RoleAssistant: {},
	RoleTool:      {},
	RoleSystem:    {},
}

// IsValidRole reports whether s is one of the canonical role values.
// Empty string returns false. Empty role on the wire is allowed
// (Validate skips the check); IsValidRole is for callers that have
// already decided "non-empty role must be one of these".
func IsValidRole(s string) bool {
	_, ok := validRoles[s]
	return ok
}
