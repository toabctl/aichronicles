package ingest

import (
	"os"
	"path/filepath"
)

// Agent describes one source-agent integration aichronicles supports.
// Today the registered agents are Claude Code (Anthropic) and Gemini
// CLI (Google). Each follows the same operational story — install
// hooks into a JSON config file, map a hook payload into an
// Envelope — so the shape is value-typed (not an interface): we
// just need data + a thin layer of agent-specific glue. Adding a
// new agent is a single Agent value plus a per-host AssembleX
// function in internal/cli.
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

// GeminiCLI is Google's open-source Gemini CLI agent. The hook
// system (packages/core/src/hooks/) is a near-clone of Claude
// Code's: same stdin-JSON wire shape, same {session_id,
// transcript_path, cwd, hook_event_name, timestamp} base input,
// same per-event extras (tool_name, tool_input, tool_response).
// Even sets CLAUDE_PROJECT_DIR env var "for compatibility."
//
// Hook events selected here match what claude-code subscribes to,
// translated to gemini's naming:
//
//   - BeforeAgent  ↔ UserPromptSubmit  (user prompt captured)
//   - AfterModel   ↔ Stop              (assistant turn finalised)
//   - AfterTool    ↔ PostToolUse       (tool result; failures
//     surfaced via tool_response.error inside AssembleGemini)
//   - SessionStart, SessionEnd: identical names, identical roles
//
// We skip BeforeTool (the equivalent of PreToolUse) for the same
// reason we skip it on claude: we only need post-tool to know
// what happened, and observing both doubles every tool event.
var GeminiCLI = Agent{
	Slug:        "gemini-cli",
	Description: "Google's Gemini CLI agent",
	HookEvents: []string{
		"BeforeAgent",
		"AfterModel",
		"AfterTool",
		"SessionStart",
		"SessionEnd",
	},
	DefaultSettingsPath: geminiCLIDefaultPath,
}

// geminiCLIDefaultPath resolves ~/.gemini/settings.json. Gemini's
// hook configuration lives there alongside the rest of the CLI's
// user-level state (oauth_creds.json, projects.json).
func geminiCLIDefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}
