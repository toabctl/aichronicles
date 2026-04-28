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
	"time"

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
	Limits        Limits        `toml:"limits"`
	Induction     Induction     `toml:"induction"`
}

// Induction controls the daemon-resident online-induction sweeper.
// Disabled by default — enabling it means the daemon will, on each
// SweepInterval tick, automatically run single-session induction
// against every idle un-induced session. That's a non-trivial amount
// of LLM spend, so we make the user opt in explicitly.
//
// Tuning rules:
//
//   - SweepInterval: how often the goroutine wakes. Default 15
//     minutes. Smaller intervals make induction "more online" but
//     don't help if the per-sweep work cap has nothing to do.
//
//   - Idle: how long a session must be quiet before it counts as
//     ended. Default 30 minutes — same definition the manual sweep
//     uses, kept in sync so behaviour doesn't drift between paths.
//
//   - MinEvents: skip sessions smaller than this. Default 5 (one
//     prompt + reply + tool round-trip).
//
//   - MaxPerSweep: bound LLM spend per tick. Default 5 — even a
//     pathological backlog of 100 idle sessions costs only ~5
//     calls per interval until it drains.
type Induction struct {
	Enabled       bool     `toml:"enabled"`
	SweepInterval Duration `toml:"sweep_interval"`
	Idle          Duration `toml:"idle"`
	MinEvents     int      `toml:"min_events"`
	MaxPerSweep   int      `toml:"max_per_sweep"`

	// SkipSummarize, when true, suppresses the phase-1
	// auto-summarize call. Defaults to false — the autonomous
	// pipeline assumes the daemon owns summarize too. Set true to
	// keep summarize manual (run `aichronicles summarize <id>`
	// yourself); the sweeper will then skip phases 2+3 for
	// sessions you haven't summarized.
	SkipSummarize bool `toml:"skip_summarize"`

	// SkipFacts, when true, suppresses the per-candidate
	// facts-induction LLM call that the sweep would otherwise fire
	// alongside the (skill+workflow merged) induction call.
	// Defaults to false: a user who has opted into
	// [induction].enabled has accepted the LLM-spend tradeoff for
	// auto-extraction; running facts on the same candidate adds
	// one LLM call per tick but completes the MIRIX semantic
	// memory layer without manual intervention. Set true if facts
	// induction is producing low-value rows for your workload.
	SkipFacts bool `toml:"skip_facts"`
}

// Limits exposes operationally-tunable defaults that previously lived
// as hardcoded constants throughout the codebase. Each field is
// optional; zero values mean "use the built-in default" so older
// config files (or empty config files) keep working unchanged.
//
// A new constant graduates to a Limits field when an operator might
// reasonably need to tune it — slow disks, low-memory boxes, slower-
// than-expected LLM providers, large enterprise transcripts, etc.
// Internal implementation knobs (response-body truncation, MCP
// buffer sizes, prompt-cache TTLs) stay as constants because tuning
// them changes the program's contract, not its operating envelope.
type Limits struct {
	// MaxEnvelopeBytes overrides the daemon's POST body cap.
	// Zero uses the built-in default (128 MiB). Real Claude
	// transcripts can carry single 50 MB+ assistant turns when a
	// large tool result is inlined; bump higher only if you
	// regularly see 413s in the daemon log.
	MaxEnvelopeBytes int `toml:"max_envelope_bytes"`

	// IngestTimeout caps the daemon round-trip the hook
	// subprocess will wait for. Zero uses the built-in default
	// (250ms — tight, by design: a wedged daemon must never
	// block the user's editing flow). Tune up if you observe
	// repeated outage notifications under sustained CPU load.
	IngestTimeout Duration `toml:"ingest_timeout"`

	// SummarizeTimeout caps a single `summarize` LLM round-trip.
	// Zero uses the built-in default (3 minutes — generous for
	// any 1k-token summary, even on slow providers).
	SummarizeTimeout Duration `toml:"summarize_timeout"`

	// ReflectTimeout caps `reflect` and `propose` LLM
	// round-trips (both share the larger budget). Zero uses the
	// built-in default (5 minutes).
	ReflectTimeout Duration `toml:"reflect_timeout"`

	// ShutdownDrainTimeout caps how long the daemon will wait
	// for in-flight POSTs to complete after SIGTERM/SIGINT.
	// Zero uses the built-in default (10 seconds — comfortably
	// under systemd's TimeoutStopSec=90s).
	ShutdownDrainTimeout Duration `toml:"shutdown_drain_timeout"`

	// SQLiteMaxOpenConns caps the connection pool the store
	// uses against the SQLite file. Zero uses the built-in
	// default (4 — modest because SQLite serializes writes
	// internally; more connections mostly means more waiting).
	// Bump only if profiling shows pool contention.
	SQLiteMaxOpenConns int `toml:"sqlite_max_open_conns"`
}

// Duration is a time.Duration that round-trips through TOML's
// string syntax (e.g. "250ms", "3m"). The standard time.Duration
// only marshals as nanoseconds, which is unfriendly to humans
// editing config files by hand.
type Duration time.Duration

// UnmarshalText parses Duration values written as Go duration
// strings ("250ms", "3m", "5m30s"). Empty input yields zero
// (interpreted as "use the default" by callers).
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("limits duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Or returns d's value if non-zero, fallback otherwise. Lets callers
// write `cfg.Limits.IngestTimeout.Or(defaultIngestTimeout)` without
// branching at every call site.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
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
//
// Symlinks at the config path are refused outright when
// api_key_command is set: an attacker with permission to rewrite a
// symlink (but not the underlying file) could race-replace the
// target after the perm check, and Lstat-then-Stat is the only way
// to close that window. We use Lstat to inspect the path itself —
// os.Stat would have followed the symlink and reported the
// target's permissions, missing the attack vector entirely.
func LoadFrom(path string) (*Config, error) {
	cfg := Default()
	info, statErr := os.Lstat(path)
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
		// Refuse symlinks: see the function-level comment.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf(
				"%s is a symlink and a provider api_key_command is set; "+
					"refuse to trust a symlinked config (race-replacement risk). "+
					"resolve the symlink and put the real file at %s",
				path, path)
		}
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
