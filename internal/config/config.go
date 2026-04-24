// Package config loads the aichronicles TOML config file. A missing
// file is not an error — the defaults returned from Load() are the
// same shape the package itself uses when no override is present.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/toabctl/aichronicles/internal/paths"
)

// Config is the top-level shape of $XDG_CONFIG_HOME/aichronicles/config.toml.
// Keep fields additive: anything new must ship a sensible default so
// older config files remain valid.
type Config struct {
	Notifications Notifications `toml:"notifications"`
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
func LoadFrom(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return &cfg, nil
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
