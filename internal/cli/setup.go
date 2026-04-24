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
)

// installedHooks is the fixed list aichronicles wires into Claude Code.
// Each event fires exactly one command — ours — which forwards to the
// daemon. Matchers are intentionally omitted so every tool invocation is
// captured.
var installedHooks = []string{
	"UserPromptSubmit",
	"Stop",
	"SessionStart",
	"SessionEnd",
	"PostToolUse",
	"PostToolUseFailure",
}

// defaultHookCommand is what we drop into settings.json's command field.
// It relies on `aichronicles` being on the user's PATH.
const defaultHookCommand = "aichronicles ingest"

func newSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install aichronicles into an AI coding agent or the OS",
	}
	cmd.AddCommand(newSetupClaudeCodeCmd())
	cmd.AddCommand(newSetupSystemdCmd())
	return cmd
}

func newSetupClaudeCodeCmd() *cobra.Command {
	var settingsPath string
	var hookCommand string
	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Install Claude Code hooks that forward events to aichroniclesd",
		Long: "Idempotently merges six hook entries (UserPromptSubmit, Stop,\n" +
			"PostToolUse, PostToolUseFailure, SessionStart, SessionEnd) into\n" +
			"the target settings.json, each pointing at `aichronicles ingest`.\n" +
			"Existing hook entries from other tools are preserved; running\n" +
			"twice is a no-op.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := settingsPath
			if path == "" {
				var err error
				path, err = defaultClaudeSettingsPath()
				if err != nil {
					return err
				}
			}
			report, err := InstallClaudeCodeHooks(path, hookCommand)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&settingsPath, "settings", "", "path to Claude Code settings.json (default: ~/.claude/settings.json)")
	cmd.Flags().StringVar(&hookCommand, "command", defaultHookCommand, "command to run from each hook")
	return cmd
}

// InstallClaudeCodeHooks merges our hook entries into the settings.json
// at path, creating the file if necessary. It returns a human-readable
// summary of what changed. Safe to run repeatedly.
func InstallClaudeCodeHooks(path, command string) (string, error) {
	if command == "" {
		command = defaultHookCommand
	}
	settings, err := readSettings(path)
	if err != nil {
		return "", err
	}

	added, alreadyPresent := mergeAllHooks(settings, installedHooks, command)

	if len(added) > 0 {
		if err := writeSettingsAtomic(path, settings); err != nil {
			return "", err
		}
	}

	return formatReport(path, added, alreadyPresent), nil
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
func mergeAllHooks(settings map[string]any, events []string, command string) (added, present []string) {
	for _, ev := range events {
		if mergeOneHook(settings, ev, command) {
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
func mergeOneHook(settings map[string]any, eventName, command string) bool {
	hooksRoot, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooksRoot = map[string]any{}
		settings["hooks"] = hooksRoot
	}

	entriesAny, _ := hooksRoot[eventName].([]any)
	if entryHasCommand(entriesAny, command) {
		return false
	}

	newEntry := map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	}
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
