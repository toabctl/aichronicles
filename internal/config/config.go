// Package config loads the aichronicles TOML config file. A missing
// file is not an error — the defaults returned from Load() are the
// same shape the package itself uses when no override is present.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/toabctl/aichronicles/internal/paths"
)

// Config is the top-level shape of $XDG_CONFIG_HOME/aichronicles/config.toml.
// Keep fields additive: anything new must ship a sensible default so
// older config files remain valid.
type Config struct {
	Notifications Notifications `toml:"notifications"`
	Capture       Capture       `toml:"capture"`
	LLM           LLM           `toml:"llm"`
}

// LLM controls how the CLI subcommands (summarize, reflect, propose)
// pick a provider and obtain API credentials. Fields are optional;
// when everything is zero the CLI falls back to ANTHROPIC_API_KEY.
type LLM struct {
	// Provider names the LLM backend used by Block B features.
	// Recognized values: "anthropic" (default), "openai". Spelled
	// in lowercase; case is normalised at parse time.
	Provider string `toml:"provider"`

	// Anthropic groups Anthropic-specific knobs (the only one today
	// is the api_key_command, but more — model overrides, beta
	// headers — can land here without touching every caller).
	Anthropic AnthropicProvider `toml:"anthropic"`

	// OpenAI groups OpenAI-specific knobs.
	OpenAI OpenAIProvider `toml:"openai"`
}

// AnthropicProvider configures the Anthropic adapter. Empty fields
// fall back to env vars and built-in defaults.
type AnthropicProvider struct {
	// APIKeyCommand is a shell command (run via `/bin/sh -c`) whose
	// stdout yields the Anthropic API key. Used only when
	// ANTHROPIC_API_KEY is unset. Typical values:
	// `secret-tool lookup service anthropic user default`,
	// `pass show anthropic/api-key`,
	// `cat ~/.config/aichronicles/anthropic-key`.
	//
	// Trailing newlines are stripped. Runs with a 10-second timeout.
	// Stderr is discarded so a chatty keyring unlock prompt cannot
	// corrupt the resolve.
	APIKeyCommand string `toml:"api_key_command"`
}

// OpenAIProvider configures the OpenAI adapter. Same shape as the
// Anthropic block; kept separate so each provider can grow its own
// knobs without polluting the other.
type OpenAIProvider struct {
	// APIKeyCommand is a shell command whose stdout yields the OpenAI
	// API key. Used only when OPENAI_API_KEY is unset. Same execution
	// rules as AnthropicProvider.APIKeyCommand.
	APIKeyCommand string `toml:"api_key_command"`
}

// HasAPIKeyCommand reports whether any provider has an api_key_command
// set — used by LoadFrom to decide whether to enforce the 0600 mode
// check on the file. Adding a new provider here makes its presence
// trip the trust-boundary check automatically.
func (l LLM) HasAPIKeyCommand() bool {
	return l.Anthropic.APIKeyCommand != "" || l.OpenAI.APIKeyCommand != ""
}

// Capture controls what the client CLI is willing to forward to the
// daemon at all. The redactor scrubs known credential patterns; the
// denylist here is the coarser hammer for whole directories the user
// does not want captured under any circumstance — NDA-covered client
// work, ephemeral spike dirs, etc.
type Capture struct {
	// DenyPaths are absolute directory paths. An envelope whose cwd
	// equals, or is a descendant of, any entry here is dropped at
	// the client before POST. Matching is purely lexicographic:
	// symlinks are NOT resolved, so list canonical paths.
	DenyPaths []string `toml:"deny_paths"`
}

// IsDenied reports whether an envelope's cwd falls under any of the
// configured deny paths. Empty cwd is never denied (no information to
// match on). Empty DenyPaths means nothing is denied.
func (c Capture) IsDenied(cwd string) bool {
	if cwd == "" || len(c.DenyPaths) == 0 {
		return false
	}
	cleanCwd := filepath.Clean(cwd)
	for _, p := range c.DenyPaths {
		if p == "" {
			continue
		}
		cleanDeny := filepath.Clean(p)
		if cleanCwd == cleanDeny {
			return true
		}
		// Component-boundary match only: /foo/bar should match
		// /foo/bar/baz but NOT /foo/barrel. The trailing separator
		// enforces the boundary.
		if strings.HasPrefix(cleanCwd, cleanDeny+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Notifications toggles individual freedesktop-notification events.
// Both default on — users who installed aichronicles want to know
// when it starts and when it can't reach the daemon.
type Notifications struct {
	// DaemonStart fires one notification each time aichroniclesd
	// finishes binding its listener.
	DaemonStart bool `toml:"daemon_start"`
	// DaemonUnreachable fires one notification per outage (rate-
	// limited elsewhere) when the CLI cannot POST to the daemon.
	DaemonUnreachable bool `toml:"daemon_unreachable"`
}

// Default returns the built-in defaults. Used both as the starting
// point for Load (so missing keys inherit defaults) and as the return
// value when no config file exists.
func Default() Config {
	return Config{
		Notifications: Notifications{
			DaemonStart:       true,
			DaemonUnreachable: true,
		},
	}
}

// Load reads the TOML config from the XDG-resolved path. Missing file
// is treated as "use defaults" — equivalent to Default().
func Load() (*Config, error) {
	path, err := paths.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	return LoadFrom(path)
}

// LoadFrom reads the TOML config from an explicit path. Used in tests
// and anywhere the caller wants to override the default location.
// Missing file → defaults; malformed file → error.
//
// When `[llm].api_key_command` is set, the file mode is checked: any
// group or world bit (mask 0077) refuses to load. Rationale: an api
// key command is a trust boundary — if anyone else on the box can
// rewrite the config, they can redirect the key to arbitrary places.
func LoadFrom(path string) (*Config, error) {
	cfg := Default()
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("stat %s: %w", path, statErr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return &cfg, nil
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.LLM.HasAPIKeyCommand() {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf(
				"%s has mode %04o but a provider api_key_command is set; "+
					"refuse to trust a world/group-accessible config. "+
					"run `chmod 600 %s` to proceed",
				path, perm, path)
		}
	}
	return &cfg, nil
}
