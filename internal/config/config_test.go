package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if !reflect.DeepEqual(*cfg, Default()) {
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
	if !reflect.DeepEqual(*cfg, Default()) {
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

func TestLoadFrom_ParsesCaptureDenyPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[capture]
deny_paths = ["/work/client-nda", "/tmp/scratch"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	want := []string{"/work/client-nda", "/tmp/scratch"}
	if !reflect.DeepEqual(cfg.Capture.DenyPaths, want) {
		t.Errorf("DenyPaths: got %v, want %v", cfg.Capture.DenyPaths, want)
	}
}

func TestCapture_IsDenied_EmptyInputsReturnFalse(t *testing.T) {
	t.Parallel()
	c := Capture{DenyPaths: []string{"/x"}}
	if c.IsDenied("") {
		t.Error("empty cwd should not be denied")
	}
	cEmpty := Capture{}
	if cEmpty.IsDenied("/x") {
		t.Error("empty deny list should allow everything")
	}
}

func TestCapture_IsDenied_ExactAndDescendantMatch(t *testing.T) {
	t.Parallel()
	c := Capture{DenyPaths: []string{"/work/client-nda"}}
	for _, cwd := range []string{
		"/work/client-nda",
		"/work/client-nda/src",
		"/work/client-nda/deep/nested/file/../dir",
	} {
		if !c.IsDenied(cwd) {
			t.Errorf("expected %q to be denied", cwd)
		}
	}
}

func TestCapture_IsDenied_ComponentBoundaryOnly(t *testing.T) {
	t.Parallel()
	// /work/client must not accidentally match /work/client-nda.
	c := Capture{DenyPaths: []string{"/work/client"}}
	for _, cwd := range []string{"/work/client-nda", "/work/clientele"} {
		if c.IsDenied(cwd) {
			t.Errorf("%q should NOT be denied by deny_paths=/work/client", cwd)
		}
	}
	if !c.IsDenied("/work/client/sub") {
		t.Error("component-boundary match should allow /work/client/sub")
	}
}

func TestCapture_IsDenied_CleansPathsBeforeCompare(t *testing.T) {
	t.Parallel()
	c := Capture{DenyPaths: []string{"/work/nda/"}}
	if !c.IsDenied("/work/nda//src/") {
		t.Error("trailing/double slashes should still match after Clean")
	}
}

func TestCapture_IsDenied_IgnoresEmptyEntries(t *testing.T) {
	t.Parallel()
	c := Capture{DenyPaths: []string{"", "/work/nda"}}
	if c.IsDenied("/any/path") {
		t.Error("empty deny_paths entry must not match everything")
	}
	if !c.IsDenied("/work/nda/x") {
		t.Error("non-empty entry after empty one should still match")
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

func TestLoadFrom_AnthropicAPIKeyCommand_Parses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[llm.anthropic]
api_key_command = "secret-tool lookup service anthropic user default"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !strings.HasPrefix(cfg.LLM.Anthropic.APIKeyCommand, "secret-tool") {
		t.Errorf("anthropic api_key_command: got %q", cfg.LLM.Anthropic.APIKeyCommand)
	}
}

func TestLoadFrom_OpenAIAPIKeyCommand_Parses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[llm]
provider = "openai"

[llm.openai]
api_key_command = "secret-tool lookup service openai user default"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.LLM.Provider != "openai" {
		t.Errorf("provider: got %q, want openai", cfg.LLM.Provider)
	}
	if !strings.HasPrefix(cfg.LLM.OpenAI.APIKeyCommand, "secret-tool") {
		t.Errorf("openai api_key_command: got %q", cfg.LLM.OpenAI.APIKeyCommand)
	}
}

func TestLoadFrom_AnthropicAPIKeyCommand_RefusesGroupReadableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[llm.anthropic]
api_key_command = "echo secret"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error on 0644 config with anthropic api_key_command")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error should suggest fix, got: %v", err)
	}
}

func TestLoadFrom_OpenAIAPIKeyCommand_RefusesGroupReadableFile(t *testing.T) {
	t.Parallel()
	// Same trust-boundary rule applies to the OpenAI block — the
	// mode check fires whenever ANY provider has a key command set.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[llm.openai]
api_key_command = "echo secret"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error on 0644 config with openai api_key_command")
	}
}

func TestLoadFrom_AnthropicAPIKeyCommand_AcceptsOwnerOnlyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[llm.anthropic]
api_key_command = "echo secret"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom on 0600: %v", err)
	}
	if cfg.LLM.Anthropic.APIKeyCommand != "echo secret" {
		t.Errorf("api_key_command: got %q", cfg.LLM.Anthropic.APIKeyCommand)
	}
}

func TestLoadFrom_BothProvidersConfigured(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[llm]
provider = "anthropic"

[llm.anthropic]
api_key_command = "echo a"

[llm.openai]
api_key_command = "echo o"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.LLM.Anthropic.APIKeyCommand != "echo a" {
		t.Errorf("anthropic: got %q", cfg.LLM.Anthropic.APIKeyCommand)
	}
	if cfg.LLM.OpenAI.APIKeyCommand != "echo o" {
		t.Errorf("openai: got %q", cfg.LLM.OpenAI.APIKeyCommand)
	}
}

// TestLoadFrom_UnlockedWhenCommandEmpty proves the mode-check only
// fires when api_key_command is set — a typical config with only
// notification settings must still load under any permissive mode.
func TestLoadFrom_UnlockedWhenCommandEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[notifications]
daemon_start = false
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadFrom(path); err != nil {
		t.Errorf("config without api_key_command should load under any mode, got %v", err)
	}
}
