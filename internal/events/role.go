package events

// RoleForKind returns the canonical Role value for a given Kind.
// A pure convenience: downstream queries can always check Kind
// directly; Role exists so cross-source queries can filter on
// "all user messages" / "all assistant turns" / etc. without
// enumerating every kind.
//
// Returns the empty string for kinds that don't map to a role
// (KindUnknown, future kinds we haven't classified). Sources call
// this when assembling an Envelope to populate Role consistently.
func RoleForKind(kind string) string {
	switch kind {
	case KindUserPrompt:
		return RoleUser
	case KindAssistantMessage:
		return RoleAssistant
	case KindToolUse, KindToolResult, KindToolFailure:
		return RoleTool
	case KindSessionStart, KindSessionEnd,
		KindCompactStart, KindCompactEnd,
		KindSubagentStart, KindSubagentStop,
		KindCwdChanged, KindInstructionsLoaded,
		KindSystemMessage, KindError:
		return RoleSystem
	default:
		return ""
	}
}
