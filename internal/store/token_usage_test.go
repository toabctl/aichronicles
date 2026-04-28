package store

import (
	"database/sql"
	"testing"
	"time"
)

// seedLLMOutput inserts one llm_outputs row with the supplied
// fields. session_id may be empty (NULL); other fields are required.
func seedLLMOutput(t *testing.T, s *Store, kind, model string, inputTok, outputTok int64, createdAt time.Time) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	in := &LLMOutput{
		SessionID:    sql.NullString{},
		Kind:         LLMOutputKind(kind),
		Model:        model,
		PromptHash:   "h-" + kind + "-" + model + "-" + createdAt.Format("20060102150405"),
		InputTokens:  sql.NullInt64{Int64: inputTok, Valid: true},
		OutputTokens: sql.NullInt64{Int64: outputTok, Valid: true},
		Body:         "{}",
		CreatedAtMs:  createdAt.UnixMilli(),
	}
	if _, _, err := SaveLLMOutput(t.Context(), tx, in); err != nil {
		t.Fatalf("seed llm_output: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestLoadTokenUsage_AggregatesByDayKindModel(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	day1 := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	// Two summary rows on day1, same model — should sum. Spaced an
	// hour apart so each gets a distinct prompt_hash (the seed
	// helper hashes the timestamp into the key) and the
	// SaveLLMOutput dedup doesn't collapse them.
	seedLLMOutput(t, s, "summary", "claude-sonnet-4-6", 100, 50, day1)
	seedLLMOutput(t, s, "summary", "claude-sonnet-4-6", 200, 80, day1.Add(time.Hour))
	// One reflect row on day1, different model — separate bucket.
	seedLLMOutput(t, s, "reflect", "gpt-4o-mini", 500, 300, day1.Add(2*time.Hour))
	// One summary row on day2 — separate day bucket.
	seedLLMOutput(t, s, "summary", "claude-sonnet-4-6", 50, 25, day2)

	got, err := LoadTokenUsage(t.Context(), s.DB(), day1.UnixMilli())
	if err != nil {
		t.Fatalf("LoadTokenUsage: %v", err)
	}
	// 3 distinct (day, kind, model) buckets.
	if len(got) != 3 {
		t.Fatalf("buckets: got %d, want 3; rows=%+v", len(got), got)
	}
	// Newest day first.
	if got[0].Day != day2.Format("2006-01-02") {
		t.Errorf("ordering: got first day %q, want %q", got[0].Day, day2.Format("2006-01-02"))
	}
	// day1 summary bucket has the two rows summed.
	var summaryDay1 *TokenUsageRow
	for i := range got {
		if got[i].Day == day1.Format("2006-01-02") && got[i].Kind == "summary" {
			summaryDay1 = &got[i]
		}
	}
	if summaryDay1 == nil {
		t.Fatal("missing day1 summary bucket")
	}
	if summaryDay1.InputTokens != 300 || summaryDay1.OutputTokens != 130 || summaryDay1.RowCount != 2 {
		t.Errorf("day1 summary aggregate: got input=%d output=%d count=%d, want 300/130/2",
			summaryDay1.InputTokens, summaryDay1.OutputTokens, summaryDay1.RowCount)
	}
}

func TestLoadTokenUsage_NullTokensCountAsZero(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	day := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Seed one row with NULL tokens — provider didn't return usage.
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	in := &LLMOutput{
		Kind:        LLMKindSummary,
		Model:       "test-model",
		PromptHash:  "h-null",
		Body:        "{}",
		CreatedAtMs: day.UnixMilli(),
		// InputTokens / OutputTokens deliberately zero-value (Valid=false).
	}
	if _, _, err := SaveLLMOutput(t.Context(), tx, in); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := LoadTokenUsage(t.Context(), s.DB(), day.UnixMilli())
	if err != nil {
		t.Fatalf("LoadTokenUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("buckets: got %d, want 1", len(got))
	}
	if got[0].InputTokens != 0 || got[0].OutputTokens != 0 || got[0].RowCount != 1 {
		t.Errorf("NULL-token row: got input=%d output=%d count=%d, want 0/0/1",
			got[0].InputTokens, got[0].OutputTokens, got[0].RowCount)
	}
}

func TestLoadTokenUsage_RespectsSinceCutoff(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	seedLLMOutput(t, s, "summary", "m", 100, 50, old)
	seedLLMOutput(t, s, "summary", "m", 200, 80, recent)

	got, err := LoadTokenUsage(t.Context(), s.DB(), cutoff.UnixMilli())
	if err != nil {
		t.Fatalf("LoadTokenUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("post-cutoff: got %d buckets, want 1", len(got))
	}
	if got[0].InputTokens != 200 {
		t.Errorf("only the recent row should count: got %d input, want 200", got[0].InputTokens)
	}
}

func TestSumTokenUsage(t *testing.T) {
	t.Parallel()
	rows := []TokenUsageRow{
		{InputTokens: 100, OutputTokens: 50, RowCount: 2},
		{InputTokens: 200, OutputTokens: 80, RowCount: 1},
		{InputTokens: 0, OutputTokens: 0, RowCount: 1},
	}
	got := SumTokenUsage(rows)
	if got.InputTokens != 300 || got.OutputTokens != 130 || got.RowCount != 4 {
		t.Errorf("totals: got %+v, want 300/130/4", got)
	}
	if (SumTokenUsage(nil) != TokenUsageTotals{}) {
		t.Error("nil input should return zero totals")
	}
}
