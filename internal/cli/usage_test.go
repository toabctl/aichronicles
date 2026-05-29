package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/pricing"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

func TestHumanInt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1_000, "1,000"},
		{12_345, "12,345"},
		{1_234_567, "1,234,567"},
		{1_000_000_000, "1,000,000,000"},
	}
	for _, c := range cases {
		if got := humanInt(c.in); got != c.want {
			t.Errorf("humanInt(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderUsage_EmptyStorePrintsPlaceholder(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := renderUsage(&buf, wire.UsageResponse{}, nil, defaultUsageWindow, FormatTable)
	if err != nil {
		t.Fatalf("renderUsage: %v", err)
	}
	if !strings.Contains(buf.String(), "no LLM calls") {
		t.Errorf("expected empty-state line, got %q", buf.String())
	}
}

func TestRenderUsage_TableWithoutPricesShowsHint(t *testing.T) {
	t.Parallel()
	resp := wire.UsageResponse{
		Days: []wire.UsageRow{
			{Day: "2026-04-28", Kind: "summary", Model: "claude-sonnet-4-6",
				InputTokens: 12_345, OutputTokens: 6_789, RowCount: 3},
		},
		Totals: wire.UsageTotals{InputTokens: 12_345, OutputTokens: 6_789, RowCount: 3},
	}
	var buf bytes.Buffer
	if err := renderUsage(&buf, resp, nil, defaultUsageWindow, FormatTable); err != nil {
		t.Fatalf("renderUsage: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"DATE", "KIND", "MODEL", "12,345", "6,789", "TOTAL", "prices.toml"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n--- output ---\n%s", want, out)
		}
	}
	headerLine := strings.SplitN(out, "\n", 2)[0]
	if strings.Contains(headerLine, "COST") {
		t.Errorf("COST column should be hidden without prices, header=%q", headerLine)
	}
}

func TestRenderUsage_TableWithPricesShowsCost(t *testing.T) {
	t.Parallel()
	resp := wire.UsageResponse{
		Days: []wire.UsageRow{
			{Day: "2026-04-28", Kind: "summary", Model: "claude-sonnet-4-6",
				InputTokens: 1_000_000, OutputTokens: 500_000, RowCount: 1},
		},
		Totals: wire.UsageTotals{InputTokens: 1_000_000, OutputTokens: 500_000, RowCount: 1},
	}
	prices := pricing.Prices{
		"claude-sonnet-4-6": {InputPerMTok: 3.00, OutputPerMTok: 15.00},
	}
	var buf bytes.Buffer
	if err := renderUsage(&buf, resp, prices, defaultUsageWindow, FormatTable); err != nil {
		t.Fatalf("renderUsage: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "COST") {
		t.Errorf("COST column missing\n--- output ---\n%s", out)
	}
	// 1M × $3 + 0.5M × $15 = $3 + $7.50 = $10.50
	if !strings.Contains(out, "$10.50") {
		t.Errorf("expected $10.50 in output, got:\n%s", out)
	}
}

func TestRenderUsage_JSONIncludesCostWhenKnown(t *testing.T) {
	t.Parallel()
	resp := wire.UsageResponse{
		Days: []wire.UsageRow{
			{Day: "2026-04-28", Kind: "summary", Model: "claude-sonnet-4-6",
				InputTokens: 1_000_000, OutputTokens: 500_000, RowCount: 1},
			{Day: "2026-04-28", Kind: "reflect", Model: "unknown-model",
				InputTokens: 100, OutputTokens: 50, RowCount: 1},
		},
		Totals: wire.UsageTotals{InputTokens: 1_000_100, OutputTokens: 500_050, RowCount: 2},
	}
	prices := pricing.Prices{
		"claude-sonnet-4-6": {InputPerMTok: 3.00, OutputPerMTok: 15.00},
	}
	var buf bytes.Buffer
	if err := renderUsage(&buf, resp, prices, 7*24*time.Hour, FormatJSON); err != nil {
		t.Fatalf("renderUsage: %v", err)
	}

	var report UsageReportJSON
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, buf.String())
	}
	if report.WindowDays != 7 {
		t.Errorf("window_days: got %d, want 7", report.WindowDays)
	}
	if len(report.Rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(report.Rows))
	}
	if report.Rows[0].CostUSD == nil || *report.Rows[0].CostUSD != 10.50 {
		t.Errorf("known-model row: cost should be 10.50, got %v", report.Rows[0].CostUSD)
	}
	if report.Rows[1].CostUSD != nil {
		t.Errorf("unknown-model row: cost should be nil, got %v", *report.Rows[1].CostUSD)
	}
	if report.TotalCost == nil || *report.TotalCost != 10.50 {
		t.Errorf("total cost: got %v, want 10.50", report.TotalCost)
	}
}

// TestLoadUsagePrices_FlagOverridesXDG covers the path-resolution
// fork: --prices wins over the XDG default.
func TestLoadUsagePrices_FlagOverridesXDG(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-prices.toml")
	if err := os.WriteFile(custom,
		[]byte("[models.\"x\"]\ninput_per_mtok=1.0\noutput_per_mtok=2.0\n"),
		0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := loadUsagePrices(custom)
	if err != nil {
		t.Fatalf("loadUsagePrices: %v", err)
	}
	if got["x"].InputPerMTok != 1.0 {
		t.Errorf("expected fixture loaded, got %+v", got)
	}
}

// TestRunUsage_EndToEnd seeds a few llm_outputs rows and runs
// runUsage against an apiForStore client, asserting that the
// expected token counts surface in the JSON output. Pricing path
// is independently tested.
func TestRunUsage_EndToEnd(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	now := time.Now().Add(-time.Hour)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		Kind:         store.LLMKindSummary,
		Model:        "test-model",
		PromptHash:   "h1",
		Body:         "{}",
		InputTokens:  ptrTo(int64(1234)),
		OutputTokens: ptrTo(int64(567)),
		CreatedAtMs:  now.UnixMilli(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	c := apiForStore(t, s)
	var out bytes.Buffer
	if err := runUsage(context.Background(), c, runUsageOpts{
		Since:      defaultUsageWindow,
		PricesPath: "/nonexistent.toml",
		Format:     FormatJSON,
		Out:        &out,
	}); err != nil {
		t.Fatalf("runUsage: %v", err)
	}

	var report UsageReportJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse JSON output: %v\nbody: %s", err, out.String())
	}
	if report.Totals.InputTokens != 1234 || report.Totals.OutputTokens != 567 {
		t.Errorf("totals: got input=%d output=%d, want 1234/567",
			report.Totals.InputTokens, report.Totals.OutputTokens)
	}
}
