package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MissingFileReturnsNilNoError(t *testing.T) {
	t.Parallel()
	p, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if p != nil {
		t.Errorf("missing file should return nil Prices, got %+v", p)
	}
}

func TestLoad_ParsesTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.toml")
	body := `
[models."claude-sonnet-4-6"]
input_per_mtok  = 3.00
output_per_mtok = 15.00

[models."gpt-4o-mini"]
input_per_mtok  = 0.15
output_per_mtok = 0.60
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := p["claude-sonnet-4-6"].InputPerMTok; got != 3.00 {
		t.Errorf("claude input: got %v, want 3.00", got)
	}
	if got := p["gpt-4o-mini"].OutputPerMTok; got != 0.60 {
		t.Errorf("openai output: got %v, want 0.60", got)
	}
}

func TestLoad_MalformedTOMLErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.toml")
	if err := os.WriteFile(path, []byte("this is not [valid toml"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected error on malformed TOML, got nil")
	}
}

func TestCostUSD(t *testing.T) {
	t.Parallel()
	p := Prices{
		"claude-sonnet-4-6": {InputPerMTok: 3.00, OutputPerMTok: 15.00},
	}
	// 1M input tokens × $3 + 0.5M output × $15 = $3 + $7.50 = $10.50
	cost, known := p.CostUSD("claude-sonnet-4-6", 1_000_000, 500_000)
	if !known {
		t.Fatal("known model should return known=true")
	}
	if math.Abs(cost-10.50) > 1e-9 {
		t.Errorf("cost: got %v, want 10.50", cost)
	}

	// Unknown model → (0, false).
	cost, known = p.CostUSD("nonsense-v1", 1_000_000, 1_000_000)
	if known || cost != 0 {
		t.Errorf("unknown model: got cost=%v known=%v, want 0/false", cost, known)
	}

	// nil receiver → (0, false), no panic.
	var nilPrices Prices
	if cost, known := nilPrices.CostUSD("claude-sonnet-4-6", 1, 1); known || cost != 0 {
		t.Errorf("nil Prices: got cost=%v known=%v, want 0/false", cost, known)
	}
}
