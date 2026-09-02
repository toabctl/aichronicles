package agents

import (
	"os"
	"path/filepath"
)

// Agent describes one source-agent integration aichronicles supports.
// Today the registered agents are Claude Code (Anthropic), Gemini
// CLI (Google) and Codex CLI (OpenAI). Each follows the same
// operational story — install
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

	// HookTimeoutsSec overrides the host's default per-hook
	// timeout, in seconds, for the named events. Absent events get
	// no `timeout` key at all, which means "whatever the host
	// defaults to" — the right answer almost always, since a
	// timeout we invent can only be wrong in one direction.
	//
	// It exists for the case where the host's default is tighter
	// than our ingest budget and the host lets us say so. Only
	// Codex needs it today (its SessionEnd default is 1s).
	HookTimeoutsSec map[string]int

	// DefaultSettingsPath returns the absolute path to the agent's
	// hooks config file (e.g. ~/.claude/settings.json for Claude
	// Code). Computed at runtime so $HOME changes between unit
	// tests and production are picked up. Must be safe to call
	// repeatedly.
	DefaultSettingsPath func() (string, error)
}

// ClaudeCode is the Claude Code agent — Anthropic's coding agent.
// The default integration: `aichronicles hook` assumes it when
// invoked without --agent.
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

// CodexCLI is OpenAI's Codex CLI agent. Like Gemini CLI, its hook
// system is a deliberate clone of Claude Code's — same
// JSON-on-stdin wire shape, the same {session_id, transcript_path,
// cwd, hook_event_name} base input, the same PascalCase event
// names, and even Claude's PascalCase tool vocabulary on
// PostToolUse (a shell call arrives as tool_name="Bash" with
// tool_input={"command": ...}, not as Codex's internal "shell"
// tool). Verified against codex-cli 0.149.1 by capturing live hook
// payloads; the wire schemas are embedded in the binary as
// draft-07 JSON Schema (`<event>.command.input`).
//
// Two Codex-specific operational notes:
//
//   - Hook config lives in its own file, $CODEX_HOME/hooks.json,
//     not in config.toml alongside the rest of Codex's settings.
//   - Codex hashes each hook command and refuses to run one it has
//     not been told to trust. After `setup codex-cli` the next
//     interactive `codex` run prompts to trust the new entries;
//     until that is answered the hooks do not fire.
//   - Codex's per-hook timeout defaults to 600s, but SessionEnd
//     defaults to 1s and is hard-capped at 3s ("clamping SessionEnd
//     hook timeout to 3s"). 1s is inside the range our own ingest
//     budget has historically overrun on a loaded box, so we ask
//     for the 3s maximum explicitly via HookTimeoutsSec. Losing a
//     session_end costs the web UI's "ended" badge and nothing
//     else — sessions.ended_at_ms is maintained by a trigger on
//     every event — so this is insurance, not a load-bearing fix.
//
// Event selection mirrors claude-code's minus PostToolUseFailure,
// which Codex has no equivalent for: its PostToolUse tool_response
// is the raw tool output string with no error channel, so a failed
// shell command is indistinguishable from a successful one at the
// hook layer. We map every PostToolUse to tool_use rather than
// sniff exit-code prose out of the output.
var CodexCLI = Agent{
	Slug:        "codex-cli",
	Description: "OpenAI's Codex CLI agent",
	HookEvents: []string{
		"UserPromptSubmit",
		"Stop",
		"SessionStart",
		"SessionEnd",
		"PostToolUse",
	},
	HookTimeoutsSec:     map[string]int{"SessionEnd": codexSessionEndMaxTimeoutSec},
	DefaultSettingsPath: codexCLIDefaultPath,
}

// codexSessionEndMaxTimeoutSec is the largest SessionEnd hook
// timeout Codex honours. Asking for more is not an error — Codex
// clamps it and warns — but asking for exactly the cap keeps that
// warning out of the user's terminal on every single session.
const codexSessionEndMaxTimeoutSec = 3

// codexCLIDefaultPath resolves $CODEX_HOME/hooks.json, falling back
// to ~/.codex/hooks.json. CODEX_HOME is Codex's documented root
// override and every other piece of its state (auth.json,
// config.toml, sessions/) moves with it, so hooks must follow.
func codexCLIDefaultPath() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "hooks.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "hooks.json"), nil
}
