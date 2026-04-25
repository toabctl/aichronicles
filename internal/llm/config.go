package llm

import (
	"context"
	"fmt"
)

// Provider identifies which backend FromConfig builds. The string
// values mirror what users type in `[llm].provider` so test helpers
// and config callers can use the same vocabulary.
type Provider string

const (
	// ProviderAnthropic is the default; routes through the Anthropic
	// Messages API.
	ProviderAnthropic Provider = "anthropic"
	// ProviderOpenAI routes through OpenAI's chat completions +
	// function-calling API.
	ProviderOpenAI Provider = "openai"
)

// Config is the runtime LLM configuration. The CLI translates the
// TOML config.LLM into this shape so the llm package stays free of
// any direct config-package dependency. Fields are additive; a zero-
// value Config picks the Anthropic provider with env-only key
// resolution.
type Config struct {
	// Provider names the backend. Empty means ProviderAnthropic.
	Provider Provider

	// Anthropic configures the Anthropic adapter. Ignored when
	// Provider is something else.
	Anthropic ProviderConfig

	// OpenAI configures the OpenAI adapter.
	OpenAI ProviderConfig
}

// ProviderConfig is the per-provider runtime configuration. Today
// only APIKeyCommand is wired; the struct exists so future per-
// provider knobs (BaseURL, MaxRetries, ModelOverride) land without
// reshaping every caller.
type ProviderConfig struct {
	// APIKeyCommand is the optional shell command whose stdout
	// yields the API key. When empty, the adapter falls back to its
	// provider-specific env var (ANTHROPIC_API_KEY / OPENAI_API_KEY).
	APIKeyCommand string
}

// FromConfig is the canonical entry point for Block B features
// constructing an LLM client. It routes to the configured provider's
// FromEnvOrCommand and returns a ready-to-use Client.
//
// An empty Config{} yields the default — Anthropic, env-only key.
func FromConfig(ctx context.Context, cfg Config) (Client, error) {
	switch cfg.Provider {
	case "", ProviderAnthropic:
		return FromEnvOrCommand(ctx, cfg.Anthropic.APIKeyCommand)
	case ProviderOpenAI:
		return openAIFromConfig(ctx, cfg.OpenAI)
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want %q or %q)",
			cfg.Provider, ProviderAnthropic, ProviderOpenAI)
	}
}

// openAIFromConfig is a stub until SDK-4 lands the real adapter.
// Returning a clear error here means the wiring is in place — the
// CLI can already accept `provider = "openai"` in config without
// crashing on dispatch.
func openAIFromConfig(_ context.Context, _ ProviderConfig) (Client, error) {
	return nil, fmt.Errorf("llm: openai provider is not yet implemented in this build")
}
