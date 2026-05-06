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

func TestFromConfig_OpenAIProvider(t *testing.T) {
	t.Setenv(OpenAIAPIKeyEnv, "test-openai-key")
	c, err := FromConfig(context.Background(), Config{Provider: ProviderOpenAI})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := c.(*OpenAI); !ok {
		t.Errorf("expected *OpenAI, got %T", c)
	}
}

func TestFromConfig_OpenAIAPIKeyCommandFallback(t *testing.T) {
	t.Setenv(OpenAIAPIKeyEnv, "")
	c, err := FromConfig(context.Background(), Config{
		Provider: ProviderOpenAI,
		OpenAI: ProviderConfig{
			APIKeyCommand: "printf 'oai-cmd-key'",
		},
	})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	o, ok := c.(*OpenAI)
	if !ok {
		t.Fatalf("expected *OpenAI, got %T", c)
	}
	if o.APIKey != "oai-cmd-key" {
		t.Errorf("APIKey: got %q, want %q", o.APIKey, "oai-cmd-key")
	}
}

func TestFromConfig_OpenAIMissingKey(t *testing.T) {
	t.Setenv(OpenAIAPIKeyEnv, "")
	_, err := FromConfig(context.Background(), Config{Provider: ProviderOpenAI})
	if err == nil {
		t.Fatal("expected error when OPENAI_API_KEY is unset and no command")
	}
	if !strings.Contains(err.Error(), OpenAIAPIKeyEnv) {
		t.Errorf("error should name the env var: %v", err)
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
