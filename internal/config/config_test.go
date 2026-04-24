package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault_BothNotificationsEnabled(t *testing.T) {
	t.Parallel()
	d := Default()
	if !d.Notifications.DaemonStart {
		t.Error("DaemonStart should default true")
	}
	if !d.Notifications.DaemonUnreachable {
		t.Error("DaemonUnreachable should default true")
	}
}

func TestLoadFrom_MissingFileReturnsDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := LoadFrom(filepath.Join(dir, "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if *cfg != Default() {
		t.Errorf("missing file should yield defaults, got %+v", cfg)
	}
}

func TestLoadFrom_EmptyFileReturnsDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if *cfg != Default() {
		t.Errorf("empty file should yield defaults, got %+v", cfg)
	}
}

func TestLoadFrom_OverridesDaemonStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[notifications]
daemon_start = false
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Notifications.DaemonStart {
		t.Error("DaemonStart should be false after override")
	}
	if !cfg.Notifications.DaemonUnreachable {
		t.Error("DaemonUnreachable should keep default true")
	}
}

func TestLoadFrom_OverridesBoth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[notifications]
daemon_start = false
daemon_unreachable = false
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Notifications.DaemonStart {
		t.Error("DaemonStart should be false")
	}
	if cfg.Notifications.DaemonUnreachable {
		t.Error("DaemonUnreachable should be false")
	}
}

func TestLoadFrom_MalformedReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this = is [not\ntoml"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error on malformed TOML")
	}
}

func TestLoadFrom_UnknownFieldIsIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[notifications]
daemon_start = false
daemon_future_event = true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// BurntSushi/toml is lenient by default; unknown keys do not error.
	// This is intentional — older binaries reading a newer config
	// should degrade gracefully.
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Notifications.DaemonStart {
		t.Error("DaemonStart should be false (override)")
	}
}
