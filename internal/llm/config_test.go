package llm

import (
	"context"
	"strings"
	"testing"
)

func TestFromConfig_DefaultProviderIsAnthropic(t *testing.T) {
	t.Setenv(APIKeyEnv, "test-key")
	c, err := FromConfig(context.Background(), Config{})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := c.(*Anthropic); !ok {
		t.Errorf("expected *Anthropic for empty config, got %T", c)
	}
}

func TestFromConfig_ExplicitAnthropic(t *testing.T) {
	t.Setenv(APIKeyEnv, "test-key")
	c, err := FromConfig(context.Background(), Config{Provider: ProviderAnthropic})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := c.(*Anthropic); !ok {
		t.Errorf("expected *Anthropic, got %T", c)
	}
}

func TestFromConfig_AnthropicAPIKeyCommandFallback(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	c, err := FromConfig(context.Background(), Config{
		Provider: ProviderAnthropic,
		Anthropic: ProviderConfig{
			APIKeyCommand: "printf 'cmd-key'",
		},
	})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	a, ok := c.(*Anthropic)
	if !ok {
		t.Fatalf("expected *Anthropic, got %T", c)
	}
	if a.APIKey != "cmd-key" {
		t.Errorf("APIKey: got %q, want %q", a.APIKey, "cmd-key")
	}
}

func TestFromConfig_OpenAINotImplementedYet(t *testing.T) {
	// Until SDK-4 lands, asking for OpenAI must error explicitly so
	// the user sees what's missing rather than getting a nil client.
	_, err := FromConfig(context.Background(), Config{Provider: ProviderOpenAI})
	if err == nil {
		t.Fatal("expected error for openai provider before SDK-4")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("error should name the provider: %v", err)
	}
}

func TestFromConfig_UnknownProviderIsError(t *testing.T) {
	_, err := FromConfig(context.Background(), Config{Provider: "google"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error should mention 'unknown provider': %v", err)
	}
	// Recommended names should appear so the user knows what's valid.
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "openai") {
		t.Errorf("error should list valid provider names: %v", err)
	}
}
