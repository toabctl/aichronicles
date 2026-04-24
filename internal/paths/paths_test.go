package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests mutate env vars and so cannot use t.Parallel.

func TestSocket_UsesXDGRuntime(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	got, err := Socket()
	if err != nil {
		t.Fatalf("Socket: %v", err)
	}
	want := "/run/user/1234/aichronicles/sock"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSocket_FallsBackToTmpWithUID(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	got, err := Socket()
	if err != nil {
		t.Fatalf("Socket: %v", err)
	}
	// Must land in TMPDIR (defaults to /tmp) and include our UID.
	if !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("fallback not under TMPDIR: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("aichronicles-%d", os.Getuid())) {
		t.Errorf("fallback missing UID component: %q", got)
	}
	if filepath.Base(got) != "sock" {
		t.Errorf("basename not sock: %q", got)
	}
}

func TestEventLog_UsesXDGState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/state/example")
	got, err := EventLog()
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	want := "/var/state/example/aichronicles/events.jsonl"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEventLog_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	got, err := EventLog()
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "state", "aichronicles", "events.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestConfigFile_UsesXDGConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/etc/xdg-example")
	got, err := ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	want := "/etc/xdg-example/aichronicles/config.toml"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestConfigFile_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "aichronicles", "config.toml")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStorePath_UsesXDGState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/state/example")
	got, err := StorePath()
	if err != nil {
		t.Fatalf("StorePath: %v", err)
	}
	want := "/var/state/example/aichronicles/store.db"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStorePath_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	got, err := StorePath()
	if err != nil {
		t.Fatalf("StorePath: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "state", "aichronicles", "store.db")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
