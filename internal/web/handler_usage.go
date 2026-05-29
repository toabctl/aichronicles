package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/pricing"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
)

// usageDefaultDays mirrors the CLI's `aichronicles usage` default —
// 30 days is the natural default for a "what did I burn this month"
// view. Configurable per-request via ?days=<n>.
const usageDefaultDays = 30

// UsagePage is the rendering shape for /usage. Mirrors what the CLI
// table renders, but pre-formatted strings + a Cost optional column
// so the template stays free of helpers.
type UsagePage struct {
	Title    string
	Days     int
	Since    string
	Until    string
	Rows     []UsageRow
	Totals   UsageTotalsRow
	HasCost  bool // controls the COST column
	Empty    bool // no llm_outputs in the window
	NoPrices bool // surfaced as a "drop a prices.toml..." hint
}

// UsageRow is one rendered table row.
type UsageRow struct {
	Day          string
	Kind         string
	Model        string
	InputTokens  string // formatted with thousands separators
	OutputTokens string
	Cost         string // empty when prices file didn't carry the model
	HasCost      bool
	RowCount     int
}

// UsageTotalsRow is the footer.
type UsageTotalsRow struct {
	InputTokens  string
	OutputTokens string
	Cost         string
	RowCount     int
}

// usageHandler renders /usage — same aggregation as `aichronicles
// usage`, as HTML. Pure SQL plus an optional TOML prices file; no
// LLM call. The COST column appears only when a row's model has a
// matching entry in $XDG_CONFIG_HOME/aichronicles/prices.toml.
func (s *Server) usageHandler(w http.ResponseWriter, r *http.Request) {
	rawDays, _ := strconv.Atoi(r.URL.Query().Get("days"))
	now := time.Now()
	sinceMs, days := timefmt.SinceMsFromDays(rawDays, usageDefaultDays, 365, now)

	resp, err := s.api.Usage(r.Context(), wire.UsageRequest{SinceMs: sinceMs})
	if err != nil {
		s.internalError(w, "usageHandler: load", "could not load usage", err)
		return
	}

	prices, perr := loadPricesForWeb()
	if perr != nil {
		// Same posture as the CLI: log and continue with no costs.
		s.log.Warn("usage: prices file unreadable, hiding COST column", "err", perr)
	}

	page := buildUsagePage(resp.Days, resp.Totals, prices, days, now)
	s.render(w, r, "usage", page)
}

// loadPricesForWeb resolves and parses the XDG-default prices file.
// Same path resolution the CLI uses; no flag override at the web
// surface (the user can edit the file or set XDG_CONFIG_HOME).
func loadPricesForWeb() (pricing.Prices, error) {
	path, err := paths.PricesFile()
	if err != nil {
		return nil, err
	}
	return pricing.Load(path)
}

func buildUsagePage(rows []wire.UsageRow, totals wire.UsageTotals, prices pricing.Prices, days int, now time.Time) UsagePage {
	page := UsagePage{
		Title: "Usage",
		Days:  days,
		Since: now.Add(-time.Duration(days) * 24 * time.Hour).UTC().Format("2006-01-02"),
		Until: now.UTC().Format("2006-01-02"),
		Empty: len(rows) == 0,
	}

	// Decide up-front whether the COST column appears. Same rule as
	// the CLI: at least one row must match a price entry, otherwise
	// hide the column entirely.
	for _, r := range rows {
		if _, known := prices.CostUSD(r.Model, r.InputTokens, r.OutputTokens); known {
			page.HasCost = true
			break
		}
	}
	page.NoPrices = !page.HasCost

	var grandCost float64
	for _, r := range rows {
		row := UsageRow{
			Day:          r.Day,
			Kind:         r.Kind,
			Model:        r.Model,
			InputTokens:  formatThousands(r.InputTokens),
			OutputTokens: formatThousands(r.OutputTokens),
			RowCount:     r.RowCount,
		}
		if cost, known := prices.CostUSD(r.Model, r.InputTokens, r.OutputTokens); known {
			row.Cost = fmt.Sprintf("$%.2f", cost)
			row.HasCost = true
			grandCost += cost
		}
		page.Rows = append(page.Rows, row)
	}
	page.Totals = UsageTotalsRow{
		InputTokens:  formatThousands(totals.InputTokens),
		OutputTokens: formatThousands(totals.OutputTokens),
		RowCount:     totals.RowCount,
	}
	if page.HasCost {
		page.Totals.Cost = fmt.Sprintf("$%.2f", grandCost)
	}
	return page
}

// formatThousands renders an int64 with comma separators. Local copy
// (rather than import-from-cli) to keep the web package free of
// upward dependencies on cli — the function is tiny.
//
// Token counts are always non-negative in practice; the sign-strip
// branch is defensive so a future caller passing a negative value
// gets "-1,234" instead of garbled output ("-,123,4").
func formatThousands(n int64) string {
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return sign + s
	}
	rem := len(s) % 3
	out := ""
	if rem > 0 {
		out = s[:rem]
		if len(s) > rem {
			out += ","
		}
	}
	for i := rem; i < len(s); i += 3 {
		out += s[i : i+3]
		if i+3 < len(s) {
			out += ","
		}
	}
	return sign + out
}
