package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

func newTeardownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove aichronicles integration from an AI coding agent or the OS",
	}
	cmd.AddCommand(newTeardownClaudeCodeCmd())
	cmd.AddCommand(newTeardownCodexCLICmd())
	cmd.AddCommand(newTeardownSystemdCmd())
	return cmd
}

func newTeardownClaudeCodeCmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	var yes bool
	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Remove aichronicles Claude Code hooks from settings.json",
		Long: "Strips every hook entry whose command matches ours from each\n" +
			"of the event types aichronicles installed into. Entries from other\n" +
			"tools are preserved unchanged. Empty event arrays and an empty\n" +
			"`hooks` object are cleaned up so the file looks pristine after\n" +
			"a full removal. Idempotent: running twice is a no-op.\n\n" +
			"Runs in dry-run mode by default: it reports what would change\n" +
			"without touching settings.json. Pass --yes to actually write.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = defaultClaudeSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := RemoveAgentHooks(ingest.ClaudeCode, path, hookCommand, !yes)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Claude Code settings.json (default: ~/.claude/settings.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommand, "command to strip from each hook")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the removal (required to modify settings.json)")
	return cmd
}

func newTeardownCodexCLICmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	var yes bool
	cmd := &cobra.Command{
		Use:   "codex-cli",
		Short: "Remove aichronicles Codex CLI hooks from hooks.json",
		Long: "Inverse of `setup codex-cli`. Strips every hook entry whose\n" +
			"command matches ours from each Codex hook event. Other tools'\n" +
			"entries are preserved; running twice is a no-op.\n\n" +
			"Dry-run by default: pass --yes to actually rewrite the file.\n" +
			"Does not touch ~/.codex/config.toml — flip\n" +
			"`[features] codex_hooks = false` yourself if you want to\n" +
			"disable hooks entirely.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = ingest.Codex.DefaultSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := RemoveAgentHooks(ingest.Codex, path, hookCommand, !yes)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Codex hooks.json (default: ~/.codex/hooks.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommandFor(ingest.Codex), "command to strip from each hook")
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
// Generic over the agent — call sites pass ingest.ClaudeCode or
// ingest.Codex and get the same merge / cleanup behaviour. The
// agent supplies which hook event names to walk.
func RemoveAgentHooks(agent ingest.Agent, path, command string, dryRun bool) (string, error) {
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
// RemoveAgentHooks directly with the desired ingest.Agent.
func RemoveClaudeCodeHooks(path, command string, dryRun bool) (string, error) {
	return RemoveAgentHooks(ingest.ClaudeCode, path, command, dryRun)
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
