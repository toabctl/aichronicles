// Package pricing parses an optional TOML table that maps Claude /
// OpenAI model identifiers to their per-million-token prices, and
// computes rough USD cost estimates from raw token counts captured
// in llm_outputs.
//
// The pricing table is opt-in: aichronicles ships no built-in price
// list, because vendor prices change without notice and an outdated
// number would mislead the user. The user runs `aichronicles usage`
// without a prices file and sees raw token totals; they drop a
// prices.toml under XDG_CONFIG_HOME/aichronicles/ and the same
// command picks up cost columns automatically.
//
// Schema (TOML):
//
//	[models."claude-sonnet-4-6"]
//	input_per_mtok  = 3.00
//	output_per_mtok = 15.00
//
//	[models."gpt-4o-mini"]
//	input_per_mtok  = 0.15
//	output_per_mtok = 0.60
//
// Currency is whatever the user's prices express — labelled USD by
// convention but the math is pure (tokens × price / 1M); no
// localisation is performed.
package pricing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/BurntSushi/toml"
)

// ModelPrice is one row of the prices table — per-million-token
// rates for a single model. Fields are in the same currency as
// whatever the user wrote in their prices.toml; aichronicles is
// currency-agnostic and just multiplies.
type ModelPrice struct {
	InputPerMTok  float64 `toml:"input_per_mtok"`
	OutputPerMTok float64 `toml:"output_per_mtok"`
}

// Prices is the parsed table — a map from model id (the same
// string llm_outputs.model holds, e.g. "claude-sonnet-4-6") to its
// price entry. A nil or empty Prices is valid; CostUSD on a nil
// receiver returns (_, false), so callers don't need a separate
// "did the file load?" branch.
type Prices map[string]ModelPrice

// Load parses path as TOML. Missing file → (nil, nil): the prices
// table is opt-in, and absence is normal. Malformed TOML or
// otherwise unreadable files surface as errors so the caller can
// log them rather than silently fall through to "no costs."
func Load(path string) (Prices, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read prices file %s: %w", path, err)
	}
	var raw struct {
		Models map[string]ModelPrice `toml:"models"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse prices file %s: %w", path, err)
	}
	return Prices(raw.Models), nil
}

// CostUSD returns the rough cost of a single (model, inputTokens,
// outputTokens) tuple. Second return is true when the model has an
// entry in the pricing table; false (with cost=0) when it doesn't —
// callers render a "-" or skip the cost column for unknown models
// rather than reporting a fabricated zero.
//
// Math: (tokens / 1_000_000) × price_per_mtok. Float64 has plenty of
// headroom for realistic personal-use totals (millions of tokens at
// double-digit dollars per Mtok).
func (p Prices) CostUSD(model string, inputTokens, outputTokens int64) (cost float64, known bool) {
	if p == nil {
		return 0, false
	}
	mp, ok := p[model]
	if !ok {
		return 0, false
	}
	cost = (float64(inputTokens)/1_000_000)*mp.InputPerMTok +
		(float64(outputTokens)/1_000_000)*mp.OutputPerMTok
	return cost, true
}
