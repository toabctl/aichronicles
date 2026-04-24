package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newTeardownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove aichronicles integration from an AI coding agent",
	}
	cmd.AddCommand(newTeardownClaudeCodeCmd())
	return cmd
}

func newTeardownClaudeCodeCmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Remove aichronicles Claude Code hooks from settings.json",
		Long: "Strips every hook entry whose command matches ours from each\n" +
			"of the event types aichronicles installed into. Entries from other\n" +
			"tools are preserved unchanged. Empty event arrays and an empty\n" +
			"`hooks` object are cleaned up so the file looks pristine after\n" +
			"a full removal. Idempotent: running twice is a no-op.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = defaultClaudeSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := RemoveClaudeCodeHooks(path, hookCommand)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Claude Code settings.json (default: ~/.claude/settings.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommand, "command to strip from each hook")
	return cmd
}

// RemoveClaudeCodeHooks is the inverse of InstallClaudeCodeHooks: it
// drops every hook entry whose inner command matches, preserves
// entries belonging to other tools, and rewrites the file only when
// something actually changed.
func RemoveClaudeCodeHooks(path, command string) (string, error) {
	if command == "" {
		command = defaultHookCommand
	}
	settings, err := readSettings(path)
	if err != nil {
		return "", err
	}

	hooksRoot, ok := settings["hooks"].(map[string]any)
	if !ok {
		// No hooks section at all — nothing to remove.
		return formatRemoveReport(path, nil), nil
	}

	var removed []string
	for _, ev := range installedHooks {
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

	if len(removed) > 0 {
		if err := writeSettingsAtomic(path, settings); err != nil {
			return "", err
		}
	}

	sort.Strings(removed)
	return formatRemoveReport(path, removed), nil
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

func formatRemoveReport(path string, removed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "settings: %s\n", path)
	if len(removed) == 0 {
		fmt.Fprint(&b, "no changes: no aichronicles hooks found")
		return b.String()
	}
	fmt.Fprintf(&b, "removed %d hook entries: %s", len(removed), strings.Join(removed, ", "))
	return b.String()
}
