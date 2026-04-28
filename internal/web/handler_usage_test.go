package web

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// TestUsagePage_EmptyStoreShowsPlaceholder covers the no-llm_outputs
// state — render the empty hint rather than a stray empty table.
func TestUsagePage_EmptyStoreShowsPlaceholder(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/usage")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200", status)
	}
	if !strings.Contains(body, "no LLM calls") {
		t.Errorf("expected empty-state line:\n%s", body)
	}
	if !strings.Contains(body, `href="/usage?days=7"`) {
		t.Errorf("window chips missing:\n%s", body)
	}
}

// TestUsagePage_RendersTokenTotals seeds a few llm_outputs and
// confirms the table picks up the per-day×kind×model rollup, the
// totals footer, and the "drop a prices.toml" hint when no prices
// file is present.
func TestUsagePage_RendersTokenTotals(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	now := time.Now().Add(-time.Hour)

	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:         store.LLMKindSummary,
		Model:        "claude-sonnet-4-6",
		PromptHash:   "h1",
		Body:         "{}",
		InputTokens:  sql.NullInt64{Int64: 12345, Valid: true},
		OutputTokens: sql.NullInt64{Int64: 6789, Valid: true},
		CreatedAtMs:  now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	base, stop := startTestServer(t, st)
	defer stop()

	status, body := fetch(t, base+"/usage")
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", status, body)
	}
	for _, want := range []string{
		"<h1>Usage", "claude-sonnet-4-6", "12,345", "6,789", "Total",
		"prices.toml", // hint shows when no prices file
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/usage body missing %q", want)
		}
	}
}

// TestUsagePage_DaysParamRespected confirms ?days=7 flows into the
// heading and chip-active class.
func TestUsagePage_DaysParamRespected(t *testing.T) {
	t.Parallel()
	st := openTempStore(t)
	base, stop := startTestServer(t, st)
	defer stop()

	_, body := fetch(t, base+"/usage?days=7")
	if !strings.Contains(body, "last 7 days") {
		t.Errorf("?days=7 should change heading:\n%s", body)
	}
	if !strings.Contains(body, `href="/usage?days=7" class="agent-chip agent-chip-active"`) {
		t.Errorf("7d chip should be active:\n%s", body)
	}
}
