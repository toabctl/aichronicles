// Package preview centralises the small "pick a one-line description
// for this session" helpers the CLI completion, web row renderer,
// and MCP tools each rewrote independently. Three nearly-identical
// snippet/preview pickers existed before; pinning the priority
// (summary topic > substantive first_prompt > muted placeholder)
// and the rune cap here keeps the surfaces visually consistent.
package preview

import "strings"

// SubstantiveMinRunes is the rune-count floor under which a first
// user_prompt is considered too short to stand in for a session
// summary. 30 filters the common follow-up fillers ("yes", "go
// ahead", "/loop", "what's next?") while keeping short-but-real
// prompts ("fix the OAuth login bug").
//
// Same bar pkg/llm/prompts uses to decide whether to skip a
// summary-less session in the meta-LLM digest.
const SubstantiveMinRunes = 30

// MutedPlaceholder is the string Pick returns when neither the
// summary topic nor the first prompt is usable. Surfaces honestly
// as "this session hasn't been summarized yet" rather than
// fabricating a description.
const MutedPlaceholder = "(no summary yet)"

// MaxOneLineRunes caps single-line previews — 120 runes balances
// "enough to recognise the content" against "fits one terminal
// line / one table cell."
const MaxOneLineRunes = 120

// PreviewKind labels which source Pick chose for the preview.
// Callers (the web row renderer especially) use it as a CSS class
// hint so the topic-derived line gets a different visual weight
// from the prompt fallback.
type PreviewKind string

const (
	KindTopic  PreviewKind = "topic"
	KindPrompt PreviewKind = "prompt"
	KindMuted  PreviewKind = "muted"
)

// Pick chooses the row's primary description text + kind label,
// in priority order:
//
//  1. summary topic — the model's distillation; highest signal.
//  2. first user_prompt — when it stands on its own (≥ SubstantiveMinRunes
//     after trim, not a slash-command).
//  3. MutedPlaceholder — honest "no summary yet" rather than a
//     misleading short prompt.
//
// Both inputs may be empty / whitespace-only; Pick handles that.
func Pick(summaryTopic, firstPrompt string) (text string, kind PreviewKind) {
	if topic := strings.TrimSpace(summaryTopic); topic != "" {
		return topic, KindTopic
	}
	if t := strings.TrimSpace(firstPrompt); IsSubstantivePrompt(t) {
		return t, KindPrompt
	}
	return MutedPlaceholder, KindMuted
}

// IsSubstantivePrompt returns true when s is long enough and
// non-trivial enough to use as a session description on its own.
// Trims whitespace, rejects pure slash-commands, requires at
// least SubstantiveMinRunes runes after trim.
//
// Pass the trimmed value to avoid a re-trim — Pick already does.
func IsSubstantivePrompt(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") && !strings.ContainsAny(s, " \n\t") {
		return false
	}
	return len([]rune(s)) >= SubstantiveMinRunes
}

// OneLine flattens whitespace (newlines, tabs, carriage returns)
// into single spaces and truncates to MaxOneLineRunes. Used by
// every snippet renderer so a single rendering bug doesn't get
// fixed in three places.
//
// Empty input returns empty string. Caller is responsible for
// the empty-state token (typically "-").
func OneLine(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range "\n\r\t" {
		s = strings.ReplaceAll(s, string(r), " ")
	}
	runes := []rune(s)
	if len(runes) <= MaxOneLineRunes {
		return s
	}
	return string(runes[:MaxOneLineRunes]) + "…"
}
