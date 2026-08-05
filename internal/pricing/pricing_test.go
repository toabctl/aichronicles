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

// TestCostUSDWithCache_PricesEachClassSeparately pins that the three
// token classes bill at their own rates.
//
// Anthropic reports cache_creation_input_tokens and
// cache_read_input_tokens separately from input_tokens, and they bill
// at 1.25x and 0.1x the base input rate respectively. Folding them
// into inputTokens would fix the count and break the cost:
// overcharging reads tenfold and undercharging writes by a quarter.
func TestCostUSDWithCache_PricesEachClassSeparately(t *testing.T) {
	t.Parallel()
	p := Prices{"m": {InputPerMTok: 10, OutputPerMTok: 20}}

	// 1M of each class, so the arithmetic reads directly.
	got, known := p.CostUSDWithCache("m", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !known {
		t.Fatal("expected a known price")
	}
	// 10 (input) + 20 (output) + 12.5 (write @1.25x) + 1 (read @0.1x)
	const want = 43.5
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// TestCostUSDWithCache_ExplicitRatesWin lets a provider whose cache
// pricing does not follow Anthropic's multipliers override them.
func TestCostUSDWithCache_ExplicitRatesWin(t *testing.T) {
	t.Parallel()
	p := Prices{"m": {
		InputPerMTok:      10,
		OutputPerMTok:     20,
		CacheWritePerMTok: 100,
		CacheReadPerMTok:  5,
	}}
	got, _ := p.CostUSDWithCache("m", 0, 0, 1_000_000, 1_000_000)
	const want = 105.0
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// TestCostUSD_StaysBackwardCompatible pins that the two-arg form is
// unchanged for callers that have no cache counters.
func TestCostUSD_StaysBackwardCompatible(t *testing.T) {
	t.Parallel()
	p := Prices{"m": {InputPerMTok: 10, OutputPerMTok: 20}}
	got, known := p.CostUSD("m", 1_000_000, 1_000_000)
	if !known || got != 30 {
		t.Errorf("CostUSD = %v (known=%v), want 30", got, known)
	}
}
