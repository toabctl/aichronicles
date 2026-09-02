package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/agents"
)

// defaultHookCommand is what we drop into settings.json's command
// field. It relies on `aichronicles` being on the user's PATH.
// The subcommand was renamed from "ingest" to "hook" with the
// aichronicles-api rearchitecture (the api now owns redaction
// and accepts the post; the CLI just translates and forwards).
const defaultHookCommand = "aichronicles hook"

// defaultHookCommandFor returns the per-agent hook command.
// Claude Code keeps the bare "aichronicles hook" form (the
// --agent default). Other agents pass --agent <slug> so RunHook
// dispatches to the right per-agent assembler.
func defaultHookCommandFor(agent agents.Agent) string {
	if agent.Slug == agents.ClaudeCode.Slug {
		return defaultHookCommand
	}
	return defaultHookCommand + " --agent " + agent.Slug
}

func newSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install aichronicles into an AI coding agent or the OS",
	}
	cmd.AddCommand(newSetupClaudeCodeCmd())
	cmd.AddCommand(newSetupGeminiCLICmd())
	cmd.AddCommand(newSetupCodexCLICmd())
	cmd.AddCommand(newSetupSystemdCmd())
	cmd.AddCommand(newSetupCronCmd())
	return cmd
}

func newSetupClaudeCodeCmd() *cobra.Command {
	var settingsPath string
	var userConfigPath string
	var hookCommand string
	var mcpCommand string
	var skipMCP bool
	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Install Claude Code hooks + the aichronicles MCP server entry",
		Long: "Two idempotent changes to TWO different files. Claude Code\n" +
			"keeps hook config and MCP-server config in different places:\n\n" +
			"  1. Hooks → ~/.claude/settings.json: merges six entries\n" +
			"     (UserPromptSubmit, Stop, PostToolUse, PostToolUseFailure,\n" +
			"     SessionStart, SessionEnd) each pointing at\n" +
			"     `aichronicles hook`.\n" +
			"  2. MCP server → ~/.claude.json: registers\n" +
			"     mcpServers.aichronicles pointing at `aichronicles\n" +
			"     mcp-serve`, so Claude can query past sessions / cached\n" +
			"     summaries / insights / skills / staleness mid-conversation.\n\n" +
			"~/.claude.json is the user-level Claude Code config (project\n" +
			"history, MCP servers, IDE state); ~/.claude/settings.json is\n" +
			"editor settings (hooks, permissions, theme). Same product,\n" +
			"two files — Claude Code reads MCP servers ONLY from the\n" +
			"former.\n\n" +
			"Existing hook + MCP entries from other tools are preserved.\n" +
			"Pass --skip-mcp if you don't want the MCP server registered.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			hookPath := settingsPath
			if hookPath == "" {
				var err error
				hookPath, err = agents.ClaudeCode.DefaultSettingsPath()
				if err != nil {
					return err
				}
			}
			userPath := userConfigPath
			if userPath == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("resolve home: %w", err)
				}
				userPath = filepath.Join(home, ".claude.json")
			}
			var mcp *MCPServerEntry
			if !skipMCP {
				mcp = &MCPServerEntry{
					Name:    aichroniclesMCPServerName,
					Command: mcpCommand,
				}
			}
			report, err := InstallClaudeCodeFull(hookPath, userPath, hookCommand, mcp)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Claude Code settings.json for HOOKS (default: ~/.claude/settings.json)")
	cmd.Flags().StringVar(&userConfigPath, "user-config", "", "path to Claude Code user-config json for MCP SERVERS (default: ~/.claude.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommand, "command to run from each hook")
	cmd.Flags().StringVar(&mcpCommand, "mcp-command", defaultMCPCommand, "command to register as the aichronicles MCP server")
	cmd.Flags().BoolVar(&skipMCP, "skip-mcp", false, "do not register the aichronicles MCP server in ~/.claude.json")
	return cmd
}

// aichroniclesMCPServerName is the identifier we register the MCP
// server under in settings.json. Stable; renaming would orphan
// any existing installs and force users to re-add the entry.
const aichroniclesMCPServerName = "aichronicles"

// defaultMCPCommand is the shell command Claude Code runs to start
// our MCP server. settings.json's mcpServers entry expands the
// command with no arguments by default; we ship the subcommand as
// part of the args field below.
const defaultMCPCommand = "aichronicles"

func newSetupGeminiCLICmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	cmd := &cobra.Command{
		Use:   "gemini-cli",
		Short: "Install Gemini CLI hooks that forward events to aichronicles-api",
		Long: "Idempotently merges five hook entries (BeforeAgent, AfterModel,\n" +
			"AfterTool, SessionStart, SessionEnd) into the target\n" +
			"settings.json, each pointing at `aichronicles hook --agent\n" +
			"gemini-cli`. Existing hook entries from other tools are\n" +
			"preserved; running twice is a no-op.\n\n" +
			"Default settings path is ~/.gemini/settings.json (user-level\n" +
			"hooks). Pass --settings to target a project-local\n" +
			"<project>/.gemini/settings.json instead.\n\n" +
			"Gemini's hook protocol is a near-clone of Claude Code's: it\n" +
			"sends the same JSON-on-stdin shape, so the same `aichronicles\n" +
			"ingest` shim handles both. Tool failures are detected from\n" +
			"AfterTool's tool_response.error field rather than via a\n" +
			"separate event name.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = agents.GeminiCLI.DefaultSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := InstallAgentHooks(agents.GeminiCLI, path, hookCommand)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Gemini settings.json (default: ~/.gemini/settings.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommandFor(agents.GeminiCLI), "command to run from each hook")
	return cmd
}

func newSetupCodexCLICmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	cmd := &cobra.Command{
		Use:   "codex-cli",
		Short: "Install Codex CLI hooks that forward events to aichronicles-api",
		Long: "Idempotently merges five hook entries (UserPromptSubmit, Stop,\n" +
			"PostToolUse, SessionStart, SessionEnd) into Codex's hooks.json,\n" +
			"each pointing at `aichronicles hook --agent codex-cli`.\n" +
			"Existing hook entries from other tools are preserved; running\n" +
			"twice is a no-op.\n\n" +
			"Default path is $CODEX_HOME/hooks.json, or ~/.codex/hooks.json\n" +
			"when CODEX_HOME is unset. Pass --settings to target a\n" +
			"project-local <repo>/.codex/hooks.json instead.\n\n" +
			"INSTALL AT ONE LAYER ONLY. Codex merges its hook layers (user,\n" +
			"project, plugin, managed) rather than letting the nearest one\n" +
			"win, so an entry in both ~/.codex/hooks.json and a repo's\n" +
			".codex/hooks.json fires our hook twice and stores every event\n" +
			"of that session twice. Codex also accepts hooks inline as\n" +
			"[hooks] in config.toml; we neither read nor write that form,\n" +
			"so a hand-written inline entry is a second layer too.\n\n" +
			"Codex's hook protocol is a clone of Claude Code's, down to the\n" +
			"PascalCase event names and the tool vocabulary (a shell call\n" +
			"arrives as tool_name=\"Bash\"), so the same translator shape\n" +
			"handles it. It has no tool-failure event and no error channel\n" +
			"on tool_response, so every PostToolUse is recorded as a plain\n" +
			"tool_use.\n\n" +
			"The SessionEnd entry carries an explicit `timeout` of 3s —\n" +
			"Codex defaults that one event to 1s (every other event gets\n" +
			"600s) and caps it at 3s, which is tight enough that a busy\n" +
			"machine can lose the event.\n\n" +
			"ONE MANUAL STEP REMAINS: Codex will not run a hook command it\n" +
			"has not been told to trust. The next interactive `codex` run\n" +
			"prompts you to review and trust the new entries — or run\n" +
			"/hooks inside Codex to review them on demand. Until you\n" +
			"accept, nothing is captured.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = agents.CodexCLI.DefaultSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := InstallAgentHooks(agents.CodexCLI, path, hookCommand)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(),
				"\nnext: start `codex` and accept the hook-trust prompt (or run "+
					"/hooks inside Codex) — Codex ignores untrusted hooks")
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Codex hooks.json (default: $CODEX_HOME/hooks.json, else ~/.codex/hooks.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommandFor(agents.CodexCLI), "command to run from each hook")
	return cmd
}

// MCPServerEntry is the shape we merge into settings.json's
// mcpServers map. Name is the key (e.g. "aichronicles"); Command
// is the executable Claude Code invokes; Args are the subcommand
// + flags passed to it. Matches Claude Code's documented schema
// at https://code.claude.com/docs/en/mcp.
type MCPServerEntry struct {
	Name    string
	Command string
	Args    []string // when nil, ["mcp-serve"] is used (the canonical aichronicles invocation)
}

// InstallClaudeCodeFull is the Claude-Code-specific superset of
// InstallAgentHooks: hook merge into settingsPath PLUS an
// mcpServers entry into userConfigPath when mcp != nil.
//
// Two files because Claude Code separates them:
//   - settingsPath    is ~/.claude/settings.json   (hooks)
//   - userConfigPath  is ~/.claude.json            (MCP servers)
//
// Each file is read, merged, and written atomically — the cross-
// file write isn't transactional, but both merges are idempotent
// so a retry after a partial failure converges. Returns a single
// human-readable report covering both mutations.
func InstallClaudeCodeFull(settingsPath, userConfigPath, hookCommand string, mcp *MCPServerEntry) (string, error) {
	if hookCommand == "" {
		hookCommand = defaultHookCommand
	}
	settings, err := readSettings(settingsPath)
	if err != nil {
		return "", err
	}
	hooksAdded, hooksPresent := mergeAllHooks(settings, agents.ClaudeCode.HookEvents, hookCommand, agents.ClaudeCode.HookTimeoutsSec)
	if len(hooksAdded) > 0 {
		if err := writeSettingsAtomic(settingsPath, settings); err != nil {
			return "", err
		}
	}

	mcpAdded := false
	mcpPresent := false
	if mcp != nil {
		userCfg, err := readSettings(userConfigPath)
		if err != nil {
			return "", err
		}
		args := mcp.Args
		if args == nil {
			args = []string{"mcp-serve"}
		}
		if mergeMCPServer(userCfg, mcp.Name, mcp.Command, args) {
			mcpAdded = true
			if err := writeSettingsAtomic(userConfigPath, userCfg); err != nil {
				return "", err
			}
		} else {
			mcpPresent = true
		}
	}

	return formatClaudeCodeReport(settingsPath, userConfigPath, hooksAdded, hooksPresent, mcpAdded, mcpPresent, mcp), nil
}

// mergeMCPServer inserts {command, args} under
// settings.mcpServers[name], leaving any other server entries
// (or any pre-existing entry of the same name) intact. Returns
// true iff settings was mutated. Conservative on conflicts: if
// the name already exists with a DIFFERENT command we treat that
// as user intent and leave it alone — running setup again must
// not silently flip a hand-edited entry.
func mergeMCPServer(settings map[string]any, name, command string, args []string) bool {
	root, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		root = map[string]any{}
		settings["mcpServers"] = root
	}
	existing, ok := root[name].(map[string]any)
	if ok {
		// Already present — check whether it's our entry, in
		// which case there's nothing to do; if it's been
		// hand-edited to a different command, leave it.
		if cmd, _ := existing["command"].(string); cmd == command {
			return false
		}
		// Different command: respect the user's edit, don't
		// overwrite. A future --force flag could change this.
		return false
	}
	argsAny := make([]any, 0, len(args))
	for _, a := range args {
		argsAny = append(argsAny, a)
	}
	root[name] = map[string]any{
		"command": command,
		"args":    argsAny,
	}
	return true
}

// formatClaudeCodeReport renders the user-facing summary of an
// InstallClaudeCodeFull run. Surfaces both files separately so
// the user can grep and `cat` them in their own follow-up checks.
func formatClaudeCodeReport(settingsPath, userConfigPath string, hooksAdded, hooksPresent []string, mcpAdded, mcpPresent bool, mcp *MCPServerEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "hook settings: %s\n", settingsPath)
	if len(hooksAdded) > 0 {
		fmt.Fprintf(&b, "added %d hook entries: %s\n", len(hooksAdded), strings.Join(hooksAdded, ", "))
	}
	if len(hooksPresent) > 0 {
		fmt.Fprintf(&b, "already present: %s\n", strings.Join(hooksPresent, ", "))
	}
	if mcp != nil {
		fmt.Fprintf(&b, "user config:   %s\n", userConfigPath)
		switch {
		case mcpAdded:
			fmt.Fprintf(&b, "registered mcpServers.%s\n", mcp.Name)
		case mcpPresent:
			fmt.Fprintf(&b, "mcpServers.%s already registered (no change)\n", mcp.Name)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// InstallAgentHooks merges aichronicles hook entries into the agent's
// settings.json at path, creating the file if necessary. Returns a
// human-readable summary of what changed. Safe to run repeatedly.
//
// The function is agent-neutral: call sites pass agents.GeminiCLI or
// agents.CodexCLI and get the same install logic. The agent supplies
// which event names to register and where the file lives; everything
// else — JSON shape, file mode, merge semantics — is shared, because
// all three agents use the same `hooks.<Event>[].hooks[]` container.
func InstallAgentHooks(agent agents.Agent, path, command string) (string, error) {
	if command == "" {
		command = defaultHookCommand
	}
	settings, err := readSettings(path)
	if err != nil {
		return "", err
	}

	added, alreadyPresent := mergeAllHooks(settings, agent.HookEvents, command, agent.HookTimeoutsSec)

	if len(added) > 0 {
		if err := writeSettingsAtomic(path, settings); err != nil {
			return "", err
		}
	}

	return formatReport(path, added, alreadyPresent), nil
}

// InstallClaudeCodeHooks is a thin wrapper kept for callers (mainly
// the test suite) that target the Claude Code agent specifically.
// New code should call InstallAgentHooks directly with the desired
// agents.Agent value.
func InstallClaudeCodeHooks(path, command string) (string, error) {
	return InstallAgentHooks(agents.ClaudeCode, path, command)
}

// readSettings returns the settings.json contents as a generic map, or
// an empty map if the file does not exist. Any other error (bad perms,
// malformed JSON) propagates.
func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// writeSettingsAtomic writes settings to path via a temp file + rename,
// creating the parent directory with 0700 and the file with 0600 if
// they did not exist yet.
func writeSettingsAtomic(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("ensure dir: %w", err)
	}
	buf, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	buf = append(buf, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := io.Copy(tmp, strings.NewReader(string(buf))); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// mergeAllHooks walks every event name and inserts our hook entry where
// missing. Returns the names that were newly added and those already
// present so the caller can report accurately.
//
// timeoutsSec is the agent's per-event timeout override map (see
// agents.Agent.HookTimeoutsSec); events absent from it get no
// `timeout` key and inherit the host's default.
func mergeAllHooks(settings map[string]any, events []string, command string, timeoutsSec map[string]int) (added, present []string) {
	for _, ev := range events {
		if mergeOneHook(settings, ev, command, timeoutsSec[ev]) {
			added = append(added, ev)
		} else {
			present = append(present, ev)
		}
	}
	sort.Strings(added)
	sort.Strings(present)
	return added, present
}

// mergeOneHook inserts {type: command, command: <command>} under
// settings.hooks[event], leaving any existing entries intact. Returns
// true iff the settings map was mutated.
//
// A positive timeoutSec adds a `timeout` key to the entry. Zero
// omits it entirely rather than writing `"timeout": 0`, which the
// hosts would read as "give up immediately" — the difference
// between "no opinion" and "the tightest possible deadline".
func mergeOneHook(settings map[string]any, eventName, command string, timeoutSec int) bool {
	hooksRoot, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooksRoot = map[string]any{}
		settings["hooks"] = hooksRoot
	}

	entriesAny, _ := hooksRoot[eventName].([]any)
	if entryHasCommand(entriesAny, command) {
		return false
	}

	exec := map[string]any{"type": "command", "command": command}
	if timeoutSec > 0 {
		exec["timeout"] = timeoutSec
	}
	newEntry := map[string]any{"hooks": []any{exec}}
	hooksRoot[eventName] = append(entriesAny, newEntry)
	return true
}

// entryHasCommand reports whether any existing entry under an event
// already invokes command — matching by the command string inside a
// hooks[].command field. Conservative: minor whitespace differences are
// treated as distinct so we don't wrongly skip a partial install.
func entryHasCommand(entries []any, command string) bool {
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := entry["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			exec, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := exec["command"].(string); cmd == command {
				return true
			}
		}
	}
	return false
}

// defaultClaudeSettingsPath resolves ~/.claude/settings.json.
func defaultClaudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func formatReport(path string, added, present []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "settings: %s\n", path)
	if len(added) == 0 {
		fmt.Fprintf(&b, "no changes: all %d aichronicles hooks already present\n", len(present))
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "added %d hook entries: %s\n", len(added), strings.Join(added, ", "))
	if len(present) > 0 {
		fmt.Fprintf(&b, "already present: %s\n", strings.Join(present, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}
