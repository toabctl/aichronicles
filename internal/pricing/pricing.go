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

	// CacheWritePerMTok / CacheReadPerMTok price Anthropic's prompt
	// cache. Both are optional: when unset, CostUSD derives them
	// from InputPerMTok using Anthropic's published multipliers, so
	// an existing price table stays correct without being rewritten.
	//
	// Set them explicitly only for a provider whose cache pricing
	// does not follow that shape.
	CacheWritePerMTok float64 `toml:"cache_write_per_mtok"`
	CacheReadPerMTok  float64 `toml:"cache_read_per_mtok"`
}

// Anthropic's published prompt-cache multipliers, relative to the
// base input rate: a cache write costs 1.25x, a cache read 0.1x.
// Used as the default when a price entry omits the cache rates.
const (
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.10
)

// CacheWriteRate returns the effective cache-write rate, defaulting
// to the multiplier off InputPerMTok.
func (mp ModelPrice) CacheWriteRate() float64 {
	if mp.CacheWritePerMTok > 0 {
		return mp.CacheWritePerMTok
	}
	return mp.InputPerMTok * cacheWriteMultiplier
}

// CacheReadRate returns the effective cache-read rate, defaulting to
// the multiplier off InputPerMTok.
func (mp ModelPrice) CacheReadRate() float64 {
	if mp.CacheReadPerMTok > 0 {
		return mp.CacheReadPerMTok
	}
	return mp.InputPerMTok * cacheReadMultiplier
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
	return p.CostUSDWithCache(model, inputTokens, outputTokens, 0, 0)
}

// CostUSDWithCache prices a call whose prompt-cache counters are
// known, billing each class at its own rate.
//
// Folding cache tokens into inputTokens would get the count right and
// the cost wrong: a cache read bills at a tenth of the base rate, so
// treating reads as ordinary input overcharges them tenfold, and
// treating writes as ordinary input undercharges them by a quarter.
// The three counters bill differently, so they are summed
// differently.
func (p Prices) CostUSDWithCache(model string, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens int64) (cost float64, known bool) {
	if p == nil {
		return 0, false
	}
	mp, ok := p[model]
	if !ok {
		return 0, false
	}
	const perMTok = 1_000_000
	cost = (float64(inputTokens)/perMTok)*mp.InputPerMTok +
		(float64(outputTokens)/perMTok)*mp.OutputPerMTok +
		(float64(cacheWriteTokens)/perMTok)*mp.CacheWriteRate() +
		(float64(cacheReadTokens)/perMTok)*mp.CacheReadRate()
	return cost, true
}
