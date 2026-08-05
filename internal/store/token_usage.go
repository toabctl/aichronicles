package store

import (
	"context"
	"database/sql"
	"fmt"
)

// TokenUsageRow is one bucket of LLM-token spend, aggregated over a
// (day, kind, model) tuple. Day is a UTC date in YYYY-MM-DD form so
// the renderer doesn't have to format and so JSON consumers can sort
// lexicographically. InputTokens / OutputTokens come from
// llm_outputs.{input,output}_tokens — both nullable per migration 002,
// so rows from providers that don't report usage contribute zero
// rather than dropping out of the aggregate.
type TokenUsageRow struct {
	Day          string `json:"day"` // "YYYY-MM-DD" UTC
	Kind         string `json:"kind"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`

	// CacheWriteTokens / CacheReadTokens are Anthropic's prompt-cache
	// counters. They are reported separately from input_tokens, so
	// they were previously invisible here — for a cache-heavy call
	// like propose verify (a few hundred user tokens against a 4 KB
	// cached system prompt) the reported usage was a small fraction
	// of the real thing.
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`

	RowCount int `json:"row_count"`
}

// LoadTokenUsage aggregates llm_outputs.input_tokens / output_tokens
// over the half-open window [sinceMs, ∞), grouped by (day, kind,
// model). One row per distinct combination, newest day first; within
// a day rows are ordered by kind then model alphabetically for stable
// display.
//
// Powers `aichronicles usage` (CLI) and the /usage web page. Counts
// rows whose tokens are NULL as contributing zero — providers that
// don't return usage data still show up in the row_count but don't
// inflate or shrink the totals.
//
// sinceMs is the inclusive lower bound on llm_outputs.created_at_ms.
// A non-positive value matches every row in the table.
func LoadTokenUsage(ctx context.Context, db *sql.DB, sinceMs int64) ([]TokenUsageRow, error) {
	const q = `
SELECT strftime('%Y-%m-%d', created_at_ms / 1000, 'unixepoch') AS day,
       kind,
       model,
       COALESCE(SUM(input_tokens), 0)        AS input_tokens,
       COALESCE(SUM(output_tokens), 0)       AS output_tokens,
       COALESCE(SUM(cache_write_tokens), 0)  AS cache_write_tokens,
       COALESCE(SUM(cache_read_tokens), 0)   AS cache_read_tokens,
       COUNT(*)                              AS row_count
  FROM llm_outputs
 WHERE created_at_ms >= ?
 GROUP BY day, kind, model
 ORDER BY day DESC, kind ASC, model ASC`
	rows, err := db.QueryContext(ctx, q, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("query token usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TokenUsageRow
	for rows.Next() {
		var r TokenUsageRow
		if err := rows.Scan(&r.Day, &r.Kind, &r.Model,
			&r.InputTokens, &r.OutputTokens,
			&r.CacheWriteTokens, &r.CacheReadTokens, &r.RowCount); err != nil {
			return nil, fmt.Errorf("scan token usage row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TokenUsageTotals is the headline summary across a TokenUsageRow
// slice. Computed Go-side rather than via a second SQL query — at
// realistic row counts (one per (day, kind, model), bounded by ~30
// days × 5 kinds × 3 models = 450) summing in Go is free and keeps
// the aggregator query single-purpose.
type TokenUsageTotals struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	RowCount         int   `json:"row_count"`
}

// SumTokenUsage rolls a per-bucket slice up to grand totals. Pure
// helper; nil input returns the zero value.
func SumTokenUsage(rows []TokenUsageRow) TokenUsageTotals {
	var t TokenUsageTotals
	for _, r := range rows {
		t.InputTokens += r.InputTokens
		t.OutputTokens += r.OutputTokens
		t.CacheWriteTokens += r.CacheWriteTokens
		t.CacheReadTokens += r.CacheReadTokens
		t.RowCount += r.RowCount
	}
	return t
}
