package ingest

import (
	"os"
	"path/filepath"
)

// Agent describes one source-agent integration aichronicles supports.
// Today the only registered agent is Claude Code; OpenAI's Codex CLI
// is on the TODO and slots in by adding another Agent value next to
// ClaudeCode below.
//
// The shape is value-typed (not an interface) because every agent
// integration follows the same operational story — install hooks
// into a JSON config file, map a hook payload into an Envelope —
// and we don't need polymorphism, just data + a thin layer of
// agent-specific glue. Putting both Agents in this file keeps the
// list of known integrations literally one screen of code.
type Agent struct {
	// Slug is the source_agent string envelopes carry. Stable;
	// changing it would orphan every existing row for this agent.
	// Must match the regexp at agentSlugPattern (validated at
	// envelope-validate time).
	Slug string

	// Description is short prose for help text and docs.
	Description string

	// HookEvents is the list of event names this agent fires hooks
	// for. The setup subcommand registers our `aichronicles ingest`
	// command for each event in this list.
	HookEvents []string

	// DefaultSettingsPath returns the absolute path to the agent's
	// hooks config file (e.g. ~/.claude/settings.json for Claude
	// Code). Computed at runtime so $HOME changes between unit
	// tests and production are picked up. Must be safe to call
	// repeatedly.
	DefaultSettingsPath func() (string, error)
}

// ClaudeCode is the Claude Code agent — Anthropic's coding agent.
// The default and currently only registered integration.
//
// Hook event names follow Claude Code's published list (see
// https://docs.claude.com/en/docs/claude-code/hooks). The events
// here are the ones aichronicles cares about; Claude Code emits
// more (PreToolUse, etc.) that we don't subscribe to today.
var ClaudeCode = Agent{
	Slug:        "claude-code",
	Description: "Claude Code (Anthropic's coding agent)",
	HookEvents: []string{
		"UserPromptSubmit",
		"Stop",
		"SessionStart",
		"SessionEnd",
		"PostToolUse",
		"PostToolUseFailure",
	},
	DefaultSettingsPath: claudeCodeDefaultPath,
}

// claudeCodeDefaultPath resolves ~/.claude/settings.json. Lives as a
// named function so it can be referenced from the var block above
// (Go disallows method values on package-scope var initializers).
func claudeCodeDefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Codex is the OpenAI Codex CLI agent. Hook event names follow the
// names documented at https://developers.openai.com/codex/hooks —
// the list mirrors the subset Claude Code exposes that maps cleanly
// to our envelope kinds. Codex requires `[features] codex_hooks =
// true` in ~/.codex/config.toml; the setup command's docs note this
// (we don't rewrite the user's config.toml for them).
//
// Implementation status: based on documented hook field shapes; the
// per-fixture validation TODO entry tracks the work to harden this
// against real Codex stdin payloads.
var Codex = Agent{
	Slug:        "codex",
	Description: "OpenAI Codex CLI",
	HookEvents: []string{
		"UserPromptSubmit",
		"Stop",
		"PostToolUse",
		"PostToolUseFailure",
	},
	DefaultSettingsPath: codexDefaultPath,
}

// codexDefaultPath resolves ~/.codex/hooks.json. Codex documents the
// hook config as living next to its config.toml in ~/.codex/.
func codexDefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "hooks.json"), nil
}
