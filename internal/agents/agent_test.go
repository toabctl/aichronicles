package agents

import (
	"path/filepath"
	"regexp"
	"testing"
)

// agentSlugPattern mirrors the regexp events.Envelope.Validate
// enforces on source_agent. Duplicated here (rather than exported
// from internal/events) so a slug that would be rejected at
// ingest-time fails at the registry instead — the slug is written
// into every row this agent ever produces, so a bad one is only
// catchable before the first install.
var agentSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

func registered() []Agent { return []Agent{ClaudeCode, GeminiCLI, CodexCLI} }

func TestRegisteredAgentsAreWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, a := range registered() {
		t.Run(a.Slug, func(t *testing.T) {
			if !agentSlugPattern.MatchString(a.Slug) {
				t.Errorf("slug %q does not match %s", a.Slug, agentSlugPattern)
			}
			if seen[a.Slug] {
				t.Errorf("duplicate slug %q", a.Slug)
			}
			seen[a.Slug] = true
			if a.Description == "" {
				t.Error("empty description")
			}
			if len(a.HookEvents) == 0 {
				t.Error("no hook events registered")
			}
			if a.DefaultSettingsPath == nil {
				t.Fatal("nil DefaultSettingsPath")
			}
			path, err := a.DefaultSettingsPath()
			if err != nil {
				t.Fatalf("DefaultSettingsPath: %v", err)
			}
			if !filepath.IsAbs(path) {
				t.Errorf("DefaultSettingsPath must be absolute, got %q", path)
			}
		})
	}
}

// TestCodexCLIDefaultPathHonoursCodexHome: every other piece of
// Codex state (auth.json, config.toml, sessions/) moves with
// CODEX_HOME, so hooks.json must follow it too — otherwise `setup
// codex-cli` writes to a file the running Codex never reads.
func TestCodexCLIDefaultPathHonoursCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	got, err := CodexCLI.DefaultSettingsPath()
	if err != nil {
		t.Fatalf("DefaultSettingsPath: %v", err)
	}
	if want := "/custom/codex/hooks.json"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCodexCLIDefaultPathFallsBackToHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "/home/someone")
	got, err := CodexCLI.DefaultSettingsPath()
	if err != nil {
		t.Fatalf("DefaultSettingsPath: %v", err)
	}
	if want := "/home/someone/.codex/hooks.json"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCodexCLIHasNoFailureEvent documents why codex-cli subscribes
// to five events where claude-code subscribes to six: Codex emits
// no PostToolUseFailure and offers no other failure channel, so
// registering one would silently never fire.
func TestCodexCLIHasNoFailureEvent(t *testing.T) {
	t.Parallel()
	for _, ev := range CodexCLI.HookEvents {
		if ev == "PostToolUseFailure" {
			t.Errorf("codex-cli must not subscribe to %s: Codex never emits it", ev)
		}
	}
}
