package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/pkg/events"
)

func newTeardownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove aichronicles integration from an AI coding agent or the OS",
	}
	cmd.AddCommand(newTeardownClaudeCodeCmd())
	cmd.AddCommand(newTeardownGeminiCLICmd())
	cmd.AddCommand(newTeardownSystemdCmd())
	cmd.AddCommand(newTeardownCronCmd())
	return cmd
}

func newTeardownClaudeCodeCmd() *cobra.Command {
	var settingsPath string
	var userConfigPath string
	var hookCommand string
	var yes bool
	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Remove aichronicles Claude Code hooks + MCP server entry",
		Long: "Strips both halves of the install:\n\n" +
			"  - hooks from ~/.claude/settings.json (every entry whose\n" +
			"    command matches ours; entries from other tools survive)\n" +
			"  - mcpServers.aichronicles from ~/.claude.json\n\n" +
			"Empty event arrays + empty `hooks` / `mcpServers` containers\n" +
			"are cleaned up so the files look pristine after a full removal.\n" +
			"Idempotent: running twice is a no-op.\n\n" +
			"Runs in dry-run mode by default: it reports what would change\n" +
			"without touching either file. Pass --yes to actually write.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			hookPath := settingsPath
			if hookPath == "" {
				var err error
				hookPath, err = defaultClaudeSettingsPath()
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
			report, err := RemoveClaudeCodeFull(hookPath, userPath, hookCommand, !yes)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Claude Code settings.json for HOOKS (default: ~/.claude/settings.json)")
	cmd.Flags().StringVar(&userConfigPath, "user-config", "", "path to Claude Code user-config json for MCP SERVERS (default: ~/.claude.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommand, "command to strip from each hook")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the removal (required to modify the files)")
	return cmd
}

// RemoveClaudeCodeFull is the Claude-Code-specific superset of
// RemoveAgentHooks: strip our hooks from settingsPath AND drop
// mcpServers.aichronicles from userConfigPath. Mirrors
// InstallClaudeCodeFull's two-file shape — the install/teardown
// pair stays symmetric.
func RemoveClaudeCodeFull(settingsPath, userConfigPath, hookCommand string, dryRun bool) (string, error) {
	if hookCommand == "" {
		hookCommand = defaultHookCommand
	}
	settings, err := readSettings(settingsPath)
	if err != nil {
		return "", err
	}
	// Snapshot for dry-run so we can compute "would remove" without
	// mutating. Strip helpers operate on the map directly.
	settingsWork := settings
	if dryRun {
		settingsWork = cloneSettings(settings)
	}
	hookRemoved := stripAgentHooks(settingsWork, events.ClaudeCode.HookEvents, hookCommand)
	if !dryRun && len(hookRemoved) > 0 {
		if err := writeSettingsAtomic(settingsPath, settingsWork); err != nil {
			return "", err
		}
	}

	userCfg, err := readSettings(userConfigPath)
	if err != nil {
		return "", err
	}
	userWork := userCfg
	if dryRun {
		userWork = cloneSettings(userCfg)
	}
	mcpRemoved := stripMCPServer(userWork, aichroniclesMCPServerName)
	if !dryRun && mcpRemoved {
		if err := writeSettingsAtomic(userConfigPath, userWork); err != nil {
			return "", err
		}
	}

	sort.Strings(hookRemoved)
	return formatClaudeCodeTeardownReport(settingsPath, userConfigPath, hookRemoved, mcpRemoved, dryRun), nil
}

// stripAgentHooks removes every hook entry whose inner hooks
// invoke `command` from each event in events. Mutates settings
// in place. Returns the names of events that had at least one
// entry removed. Empty event arrays are deleted; an empty hooks
// map is deleted from settings.
//
// Extracted from RemoveAgentHooks so InstallClaudeCodeFull and
// RemoveClaudeCodeFull can drop the MCP entry alongside the
// hooks in a single read+write cycle.
func stripAgentHooks(settings map[string]any, events []string, command string) []string {
	hooksRoot, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	var removed []string
	for _, ev := range events {
		entriesAny, ok := hooksRoot[ev].([]any)
		if !ok {
			continue
		}
		filtered, changed := stripOurEntries(entriesAny, command)
		if !changed {
			continue
		}
		removed = append(removed, ev)
		if len(filtered) == 0 {
			delete(hooksRoot, ev)
		} else {
			hooksRoot[ev] = filtered
		}
	}
	if len(hooksRoot) == 0 {
		delete(settings, "hooks")
	}
	return removed
}

// stripMCPServer drops settings.mcpServers[name] if it exists.
// Empty mcpServers map is deleted from settings. Returns true iff
// settings was mutated.
func stripMCPServer(settings map[string]any, name string) bool {
	root, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := root[name]; !exists {
		return false
	}
	delete(root, name)
	if len(root) == 0 {
		delete(settings, "mcpServers")
	}
	return true
}

// cloneSettings returns a deep-enough copy of a settings map for
// dry-run preview. We only mutate the top-level "hooks" /
// "mcpServers" subtrees, so a one-level-deep copy of those is
// sufficient — sibling keys (security, etc.) survive untouched.
func cloneSettings(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch v := v.(type) {
		case map[string]any:
			inner := make(map[string]any, len(v))
			for kk, vv := range v {
				inner[kk] = vv
			}
			out[k] = inner
		default:
			out[k] = v
		}
	}
	return out
}

// formatClaudeCodeTeardownReport renders the user-facing summary
// of a RemoveClaudeCodeFull run. Names both files so the user can
// inspect each independently afterwards.
func formatClaudeCodeTeardownReport(settingsPath, userConfigPath string, hooksRemoved []string, mcpRemoved bool, dryRun bool) string {
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	if len(hooksRemoved) == 0 && !mcpRemoved {
		return fmt.Sprintf("hook settings: %s\nuser config:   %s\n  nothing to remove (idempotent)",
			settingsPath, userConfigPath)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "hook settings: %s\n", settingsPath)
	if len(hooksRemoved) > 0 {
		fmt.Fprintf(&b, "  %s %d hook entries: %s\n",
			verb, len(hooksRemoved), strings.Join(hooksRemoved, ", "))
	}
	fmt.Fprintf(&b, "user config:   %s\n", userConfigPath)
	if mcpRemoved {
		fmt.Fprintf(&b, "  %s mcpServers.%s\n", verb, aichroniclesMCPServerName)
	}
	if dryRun {
		b.WriteString("\n(dry-run; pass --yes to actually rewrite)")
	}
	return strings.TrimRight(b.String(), "\n")
}

func newTeardownGeminiCLICmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	var yes bool
	cmd := &cobra.Command{
		Use:   "gemini-cli",
		Short: "Remove aichronicles Gemini CLI hooks from settings.json",
		Long: "Inverse of `setup gemini-cli`. Strips every hook entry whose\n" +
			"command matches ours from each Gemini hook event. Other tools'\n" +
			"entries are preserved; running twice is a no-op.\n\n" +
			"Dry-run by default: pass --yes to actually rewrite the file.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = events.GeminiCLI.DefaultSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := RemoveAgentHooks(events.GeminiCLI, path, hookCommand, !yes)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Gemini settings.json (default: ~/.gemini/settings.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommandFor(events.GeminiCLI), "command to strip from each hook")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the removal (required to modify the file)")
	return cmd
}

// RemoveAgentHooks is the inverse of InstallAgentHooks: it drops
// every hook entry whose inner command matches, preserves entries
// belonging to other tools, and rewrites the file only when
// something actually changed.
//
// When dryRun is true the function computes the plan without touching
// the settings file. The returned report uses "would remove" phrasing
// so cobra can relay it as a preview.
//
// Generic over the agent — call sites pass events.ClaudeCode or
// events.Codex and get the same merge / cleanup behaviour. The
// agent supplies which hook event names to walk.
func RemoveAgentHooks(agent events.Agent, path, command string, dryRun bool) (string, error) {
	if command == "" {
		command = defaultHookCommandFor(agent)
	}
	settings, err := readSettings(path)
	if err != nil {
		return "", err
	}

	hooksRoot, ok := settings["hooks"].(map[string]any)
	if !ok {
		// No hooks section at all — nothing to remove.
		return formatRemoveReport(path, nil, dryRun), nil
	}

	var removed []string
	for _, ev := range agent.HookEvents {
		entriesAny, ok := hooksRoot[ev].([]any)
		if !ok {
			continue
		}
		filtered, changed := stripOurEntries(entriesAny, command)
		if !changed {
			continue
		}
		removed = append(removed, ev)
		if len(filtered) == 0 {
			delete(hooksRoot, ev)
		} else {
			hooksRoot[ev] = filtered
		}
	}

	// Clean up an empty hooks section so the file looks the way it
	// did before any setup ran.
	if len(hooksRoot) == 0 {
		delete(settings, "hooks")
	}

	if len(removed) > 0 && !dryRun {
		if err := writeSettingsAtomic(path, settings); err != nil {
			return "", err
		}
	}

	sort.Strings(removed)
	return formatRemoveReport(path, removed, dryRun), nil
}

// RemoveClaudeCodeHooks is preserved as a thin alias so existing
// callers (mainly tests) compile unchanged. New code should call
// RemoveAgentHooks directly with the desired events.Agent.
func RemoveClaudeCodeHooks(path, command string, dryRun bool) (string, error) {
	return RemoveAgentHooks(events.ClaudeCode, path, command, dryRun)
}

// stripOurEntries returns a copy of entries with every entry whose
// inner hooks all invoke our command removed. An entry that mixes our
// command with another tool's is kept intact so we never silently
// mutate someone else's integration.
func stripOurEntries(entries []any, command string) (filtered []any, changed bool) {
	filtered = make([]any, 0, len(entries))
	for _, raw := range entries {
		if isPureOurEntry(raw, command) {
			changed = true
			continue
		}
		filtered = append(filtered, raw)
	}
	return filtered, changed
}

// isPureOurEntry reports true iff raw is a hook entry whose inner hooks
// list is non-empty and every exec's command equals ours.
func isPureOurEntry(raw any, command string) bool {
	entry, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := entry["hooks"].([]any)
	if !ok || len(inner) == 0 {
		return false
	}
	for _, h := range inner {
		exec, ok := h.(map[string]any)
		if !ok {
			return false
		}
		if cmd, _ := exec["command"].(string); cmd != command {
			return false
		}
	}
	return true
}

func formatRemoveReport(path string, removed []string, dryRun bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "settings: %s\n", path)
	if len(removed) == 0 {
		fmt.Fprint(&b, "no changes: no aichronicles hooks found")
		return b.String()
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	fmt.Fprintf(&b, "%s %d hook entries: %s", verb, len(removed), strings.Join(removed, ", "))
	if dryRun {
		fmt.Fprint(&b, "\n(dry-run — pass --yes to apply)")
	}
	return b.String()
}
