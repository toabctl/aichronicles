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
	MetaAnalysis  MetaAnalysis  `toml:"meta_analysis"`
}

// MetaAnalysis controls the `aichronicles meta sweep` subcommand,
// which is driven by the aichronicles-cron-meta-analysis.timer
// systemd unit. Unlike per-session induction (driven by "session
// settled"), the meta-analyses (propose, reflect, challenge, weekly
// digest, skill revision) are time-driven — they fire on a fixed
// cadence per kind.
//
// Whether the sweep runs at all is controlled by the timer
// (`systemctl --user enable aichronicles-cron-meta-analysis.timer`),
// not by a config flag. The poll cadence (how often the timer fires
// to check for overdue kinds) lives on the timer itself; this
// struct only configures the per-kind cadence and dispatch knobs.
//
// Cadences are absolute and read off the most-recent persisted row
// of each kind, so a missed window is automatically picked up on
// the next firing.
//
// Per-kind Skip flags are independent: a user can run propose
// auto-fired but keep weekly digest manual, or vice versa.
type MetaAnalysis struct {
	// ProposeCadence / ProposeSkip control the ad-hoc propose
	// path. Zero cadence falls back to the built-in default
	// (24h). Set Skip=true to disable just this kind without
	// disabling the timer.
	ProposeCadence     Duration `toml:"propose_cadence"`
	ProposeSkip        bool     `toml:"propose_skip"`
	ProposeSinceWindow Duration `toml:"propose_since"`
	ProposeLimit       int      `toml:"propose_limit"`

	// ReflectCadence / ReflectSkip control the ad-hoc reflect
	// path. Default cadence: 7d (matching the prompt's natural
	// horizon — running it more often produces near-identical
	// retrospectives).
	ReflectCadence     Duration `toml:"reflect_cadence"`
	ReflectSkip        bool     `toml:"reflect_skip"`
	ReflectSinceWindow Duration `toml:"reflect_since"`
	ReflectLimit       int      `toml:"reflect_limit"`

	// ChallengeCadence / ChallengeSkip control the forward-
	// looking (Voyager-style curriculum) variant of propose.
	// Default cadence: 7d.
	ChallengeCadence     Duration `toml:"challenge_cadence"`
	ChallengeSkip        bool     `toml:"challenge_skip"`
	ChallengeSinceWindow Duration `toml:"challenge_since"`
	ChallengeLimit       int      `toml:"challenge_limit"`

	// ReflectWeeklyCadence / ReflectWeeklySkip control the
	// weekly digest. Default cadence: 7d. The period is anchored
	// to the previous completed Monday-00:00-UTC week.
	ReflectWeeklyCadence Duration `toml:"reflect_weekly_cadence"`
	ReflectWeeklySkip    bool     `toml:"reflect_weekly_skip"`

	// SkillRevisionCadence / SkillRevisionSkip gate the per-
	// stale-skill evolve pass. Default cadence: 24h. MinRate
	// (in [0,1]) filters which stale-correlated skills are
	// eligible — defaults to 0.5 so only skills failing more
	// than half their loads get auto-revised. Max caps the
	// number of skills revised per overdue tick (default 5).
	SkillRevisionCadence Duration `toml:"skill_revision_cadence"`
	SkillRevisionSkip    bool     `toml:"skill_revision_skip"`
	SkillRevisionMinRate float64  `toml:"skill_revision_min_rate"`
	SkillRevisionSince   Duration `toml:"skill_revision_since"`
	SkillRevisionWindow  Duration `toml:"skill_revision_window"`
	SkillRevisionMax     int      `toml:"skill_revision_max"`

	// Model, when non-empty, overrides the LLM model id for
	// every call this sweep makes. Empty falls back to the
	// provider's default.
	Model string `toml:"model"`
}

// Induction controls the `aichronicles induction sweep` subcommand,
// which is driven by the aichronicles-cron-induction.timer systemd
// unit. Whether the sweep runs at all is controlled by the timer
// (`systemctl --user enable aichronicles-cron-induction.timer`),
// not by a config flag. The wake cadence lives on the timer itself
// (OnUnitInactiveSec=15min by default).
//
// Tuning rules:
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
	Idle        Duration `toml:"idle"`
	MinEvents   int      `toml:"min_events"`
	MaxPerSweep int      `toml:"max_per_sweep"`

	// SkipSummarize, when true, suppresses the phase-1
	// auto-summarize call. Defaults to false — the autonomous
	// pipeline assumes the sweep owns summarize too. Set true to
	// keep summarize manual (run `aichronicles summarize <id>`
	// yourself); the sweeper will then skip phases 2+3 for
	// sessions you haven't summarized.
	SkipSummarize bool `toml:"skip_summarize"`

	// SkipFacts, when true, suppresses the per-candidate
	// facts-induction LLM call that the sweep would otherwise fire
	// alongside the (skill+workflow merged) induction call.
	// Defaults to false: a user who has enabled the timer has
	// accepted the LLM-spend tradeoff for auto-extraction; running
	// facts on the same candidate adds one LLM call per tick but
	// completes the MIRIX semantic memory layer without manual
	// intervention. Set true if facts induction is producing
	// low-value rows for your workload.
	SkipFacts bool `toml:"skip_facts"`

	// SkipEpisodes, when true, suppresses the per-candidate
	// episode segmentation phase (cheap local-only work — no LLM
	// call). Defaults to false: episode-keyed retrieval (Pink et
	// al., 2026 — arXiv:2502.06975) needs the segmenter to have
	// run on every settled session. Set true to defer segmentation
	// to manual control.
	SkipEpisodes bool `toml:"skip_episodes"`
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

	// IngestQueueMax caps how many rows can sit in ingest_pending
	// at once before the daemon returns 503 to new POSTs. Zero
	// uses the built-in default (10000 — generous for a personal-
	// use deployment; a sustained backlog this large signals the
	// worker is hopelessly behind and the daemon should push back
	// rather than continue to accept). The 503 surfaces to the
	// hook as a transport error and trips the outage path, so the
	// operator sees the load rather than a silent buildup.
	//
	// (Two-phase async ingest is always on — see
	// internal/api/handler_writes.go. The async_ingest flag that
	// once gated it was dropped once the worker proved itself
	// locally.)
	IngestQueueMax int `toml:"ingest_queue_max"`
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
