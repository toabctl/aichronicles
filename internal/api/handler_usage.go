package api

import (
	"net/http"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleUsage serves GET /v1/usage. Aggregates llm_outputs token
// columns over the (day, kind, model) tuple in a window and ships
// the per-bucket rows + grand totals on one response so callers
// (CLI, web, jq pipelines) don't need to re-sum.
//
// since_ms is optional; non-positive / missing values match every
// row in the table. Pricing is a client concern: the api never
// emits cost figures because the price list lives in the user's
// $XDG_CONFIG_HOME/aichronicles/prices.toml, not in the daemon.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	sinceMs, ok := parseInt64Query(w, r, "since_ms")
	if !ok {
		return
	}
	req := wire.UsageRequest{SinceMs: sinceMs}

	rows, err := store.LoadTokenUsage(r.Context(), s.store.DB(), req.SinceMs)
	if err != nil {
		s.storeError(w, "LoadTokenUsage", err)
		return
	}
	totals := store.SumTokenUsage(rows)

	out := wire.UsageResponse{
		Days: make([]wire.UsageRow, 0, len(rows)),
		Totals: wire.UsageTotals{
			InputTokens:      totals.InputTokens,
			OutputTokens:     totals.OutputTokens,
			CacheWriteTokens: totals.CacheWriteTokens,
			CacheReadTokens:  totals.CacheReadTokens,
			RowCount:         totals.RowCount,
		},
	}
	for _, r := range rows {
		out.Days = append(out.Days, wire.UsageRow{
			Day:              r.Day,
			Kind:             r.Kind,
			Model:            r.Model,
			InputTokens:      r.InputTokens,
			OutputTokens:     r.OutputTokens,
			CacheWriteTokens: r.CacheWriteTokens,
			CacheReadTokens:  r.CacheReadTokens,
			RowCount:         r.RowCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
