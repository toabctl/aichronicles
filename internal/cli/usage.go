package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/pricing"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultUsageWindow matches what TODO.md describes for the
// `aichronicles usage` command — 30 days is enough to spot a
// trend, short enough to fit in a single terminal scroll.
const defaultUsageWindow = 30 * 24 * time.Hour

func newUsageCmd() *cobra.Command {
	var (
		since      time.Duration
		sockFlag   string
		pricesPath string
		formatIn   string
	)
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Per-day LLM token totals (input/output) by kind and model",
		Long: "Aggregates llm_outputs.input_tokens / output_tokens by\n" +
			"day × kind × model so you can answer 'how many tokens did I\n" +
			"burn last week, and on what?' without dropping to SQL.\n\n" +
			"Optional cost estimation: drop a TOML at\n" +
			"$XDG_CONFIG_HOME/aichronicles/prices.toml with the per-Mtok\n" +
			"rates for your models and the table grows a COST column. No\n" +
			"file = no cost column (aichronicles ships no built-in price\n" +
			"list — vendor prices change too often to bake in).\n\n" +
			"Schema for prices.toml:\n\n" +
			"  [models.\"claude-sonnet-4-6\"]\n" +
			"  input_per_mtok  = 3.00\n" +
			"  output_per_mtok = 15.00\n\n" +
			"--format=json emits the rows + totals as a structured\n" +
			"payload suitable for jq.\n\n" +
			"Talks to aichronicles-api over its UDS (override with\n" +
			"--socket or $AICHRONICLES_API_SOCKET).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			window := since
			if window <= 0 {
				window = defaultUsageWindow
			}
			return runUsage(cmd.Context(), c, runUsageOpts{
				Since:      window,
				PricesPath: pricesPath,
				Format:     format,
				Out:        cmd.OutOrStdout(),
			})
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultUsageWindow,
		"only consider llm_outputs within this window (e.g. 7d, 30d, 24h)")
	addSocketFlag(cmd, &sockFlag)
	cmd.Flags().StringVar(&pricesPath, "prices", "",
		"path to prices.toml (default: $XDG_CONFIG_HOME/aichronicles/prices.toml)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// runUsageOpts groups runUsage's parameters. Tests can construct
// one directly and bypass cobra.
type runUsageOpts struct {
	Since      time.Duration
	PricesPath string
	Format     OutputFormat
	Out        io.Writer
}

func runUsage(ctx context.Context, c *apiclient.Client, opts runUsageOpts) error {
	sinceMs := time.Now().Add(-opts.Since).UnixMilli()
	resp, err := c.Usage(ctx, wire.UsageRequest{SinceMs: sinceMs})
	if err != nil {
		return fmt.Errorf("load token usage: %w", err)
	}

	prices, err := loadUsagePrices(opts.PricesPath)
	if err != nil {
		// Loading-error means the file existed but didn't parse —
		// surface as a warning so the user can fix their TOML, but
		// don't fail the command (raw token totals are still
		// useful).
		slog.Warn("usage: prices file unreadable, hiding COST column",
			"path", opts.PricesPath, "err", err)
		prices = nil
	}
	return renderUsage(opts.Out, resp, prices, opts.Since, opts.Format)
}

// loadUsagePrices resolves the prices file (CLI flag > XDG default)
// and returns the parsed table. A nil result + nil error means
// "no file found, render without cost column."
func loadUsagePrices(flag string) (pricing.Prices, error) {
	path := flag
	if path == "" {
		p, err := paths.PricesFile()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return pricing.Load(path)
}

// UsageRowJSON is the per-row JSON shape emitted by --format=json.
// Mirrors wire.UsageRow + an optional cost field; cost is omitted
// (nil pointer) when prices.toml didn't carry an entry for the
// model.
type UsageRowJSON struct {
	Day          string   `json:"day"`
	Kind         string   `json:"kind"`
	Model        string   `json:"model"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	RowCount     int      `json:"row_count"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
}

// UsageReportJSON is the top-level payload — rows + totals — so jq
// pipelines can grab `.totals.input_tokens` directly without
// re-summing.
type UsageReportJSON struct {
	WindowDays int              `json:"window_days"`
	Rows       []UsageRowJSON   `json:"rows"`
	Totals     wire.UsageTotals `json:"totals"`
	TotalCost  *float64         `json:"total_cost_usd,omitempty"`
}

// renderUsage writes the per-day×kind×model token report. Table
// mode renders aligned columns + a totals footer; JSON mode emits
// UsageReportJSON for jq.
func renderUsage(out io.Writer, resp wire.UsageResponse, prices pricing.Prices, window time.Duration, format OutputFormat) error {
	windowDays := int(window / (24 * time.Hour))

	if format == FormatJSON {
		return renderUsageJSON(out, resp, prices, windowDays)
	}
	return renderUsageTable(out, resp, prices, window)
}

func renderUsageJSON(out io.Writer, resp wire.UsageResponse, prices pricing.Prices, windowDays int) error {
	report := UsageReportJSON{
		WindowDays: windowDays,
		Rows:       make([]UsageRowJSON, 0, len(resp.Rows)),
		Totals:     resp.Totals,
	}
	var totalCost float64
	var anyCost bool
	for _, r := range resp.Rows {
		jr := UsageRowJSON{
			Day:          r.Day,
			Kind:         r.Kind,
			Model:        r.Model,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			RowCount:     r.RowCount,
		}
		if cost, known := prices.CostUSD(r.Model, r.InputTokens, r.OutputTokens); known {
			c := cost
			jr.CostUSD = &c
			totalCost += cost
			anyCost = true
		}
		report.Rows = append(report.Rows, jr)
	}
	if anyCost {
		report.TotalCost = &totalCost
	}
	return emitJSON(out, report)
}

func renderUsageTable(out io.Writer, resp wire.UsageResponse, prices pricing.Prices, window time.Duration) error {
	rows := resp.Rows
	totals := resp.Totals
	if len(rows) == 0 {
		_, err := fmt.Fprintf(out, "(no LLM calls in the last %s)\n", humanDuration(window))
		return err
	}

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)

	// COST column appears only when at least one row matched a
	// price entry — keeps the layout clean for users without a
	// prices.toml.
	var grandCost float64
	var anyCost bool
	for _, r := range rows {
		if _, known := prices.CostUSD(r.Model, r.InputTokens, r.OutputTokens); known {
			anyCost = true
			break
		}
	}

	if anyCost {
		_, _ = fmt.Fprintln(tw, "DATE\tKIND\tMODEL\tINPUT\tOUTPUT\tCOST\tCALLS")
	} else {
		_, _ = fmt.Fprintln(tw, "DATE\tKIND\tMODEL\tINPUT\tOUTPUT\tCALLS")
	}
	for _, r := range rows {
		if anyCost {
			cost, known := prices.CostUSD(r.Model, r.InputTokens, r.OutputTokens)
			costCol := "-"
			if known {
				costCol = fmt.Sprintf("$%.2f", cost)
				grandCost += cost
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				r.Day, r.Kind, r.Model,
				humanInt(r.InputTokens), humanInt(r.OutputTokens),
				costCol, r.RowCount)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
				r.Day, r.Kind, r.Model,
				humanInt(r.InputTokens), humanInt(r.OutputTokens),
				r.RowCount)
		}
	}
	// Footer: grand totals.
	if anyCost {
		_, _ = fmt.Fprintf(tw, "TOTAL\t\t\t%s\t%s\t$%.2f\t%d\n",
			humanInt(totals.InputTokens), humanInt(totals.OutputTokens),
			grandCost, totals.RowCount)
	} else {
		_, _ = fmt.Fprintf(tw, "TOTAL\t\t\t%s\t%s\t%d\n",
			humanInt(totals.InputTokens), humanInt(totals.OutputTokens),
			totals.RowCount)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !anyCost {
		_, _ = fmt.Fprintln(&buf)
		_, _ = fmt.Fprintln(&buf, "Tip: drop a prices.toml under $XDG_CONFIG_HOME/aichronicles/")
		_, _ = fmt.Fprintln(&buf, "     to add a COST column. See `aichronicles usage --help`.")
	}
	_, err := io.Copy(out, &buf)
	return err
}

// humanInt renders an int64 with comma thousands separators so
// the INPUT/OUTPUT columns stay readable at million-token scales.
func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
		if len(s) > rem {
			b.WriteByte(',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}
