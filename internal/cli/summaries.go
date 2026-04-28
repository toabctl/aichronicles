package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// newSummariesCmd is the `summaries` subcommand tree.
//
//	list    — scannable recent history across all output kinds
//	show    — render one stored body via the human renderer
//	missing — list sessions in a window that have no summary
//	fill    — summarize the missing sessions in a window (LLM)
//
// `missing` + `fill` exist because reflect/propose became
// mandatory-summary in 9746cef; before then a user could let
// summaries pile up un-noticed, run reflect, and get an
// underwhelming output. `missing` makes the gap visible; `fill`
// closes it.
func newSummariesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "summaries",
		Short: "Inspect stored LLM outputs (summaries, reflections, proposals)",
	}
	cmd.AddCommand(newSummariesListCmd())
	cmd.AddCommand(newSummariesShowCmd())
	cmd.AddCommand(newSummariesMissingCmd())
	cmd.AddCommand(newSummariesFillCmd())
	return cmd
}

// defaultSummariesWindow is the --since default for missing/fill.
// Three days because the typical workflow is "I worked over the
// last few days; summarise anything that didn't get caught" —
// a tight default keeps the fill cheap by default and matches
// the muted-placeholder UI on the web sessions list (rows show
// "(no summary yet)" until they're summarised, and the user wants
// a quick command to clear the recent ones).
//
// Reflect / propose default to longer windows (14d / 30d) on
// purpose — they need a wider lookback to spot cross-session
// patterns. Summaries fill is the ingredient, not the analysis.
//
// Override for one-off backfills (e.g. --since 720h for the
// last 30 days).
const defaultSummariesWindow = 3 * 24 * time.Hour

func newSummariesMissingCmd() *cobra.Command {
	var (
		since    time.Duration
		limit    int
		cwd      string
		agent    string
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "missing",
		Short: "List sessions in the window that have no cached summary",
		Long: "Reads the sessions table for entries whose ended_at falls\n" +
			"within --since AND that have no llm_outputs row of\n" +
			"kind='summary'. Useful as the first step before reflect or\n" +
			"propose, both of which now require summaries on every\n" +
			"input session (see commit 9746cef).\n\n" +
			"Read-only: no LLM calls. Pipe `--format=json | jq -r '.[].id'`\n" +
			"into `aichronicles summarize` for a manual fill, or use\n" +
			"`aichronicles summaries fill` to do it in one shot.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			sinceMs := time.Now().Add(-since).UnixMilli()
			rows, err := store.LoadSessionsMissingSummary(cmd.Context(), s.DB(),
				sinceMs, store.SessionFilter{Cwd: cwd, Agent: agent}, limit)
			if err != nil {
				return fmt.Errorf("summaries missing: %w", err)
			}
			return writeMissingSummaries(cmd.OutOrStdout(), rows, format)
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultSummariesWindow,
		"only consider sessions whose ended_at is within this window (e.g. 24h, 7d)")
	cmd.Flags().IntVar(&limit, "limit", 200, "max sessions to list")
	cmd.Flags().StringVar(&cwd, "cwd", "", "filter by exact cwd")
	cmd.Flags().StringVar(&agent, "agent", "", "filter by source_agent (claude-code | codex)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newSummariesFillCmd() *cobra.Command {
	var (
		since    time.Duration
		limit    int
		cwd      string
		agent    string
		model    string
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "fill",
		Short: "Summarize every session in the window that has no cached summary",
		Long: "Iterates the missing-summary list (see `summaries missing`)\n" +
			"and calls summarize on each entry. Sequential: one LLM call\n" +
			"at a time. Per-session failures (rate limits, malformed\n" +
			"sessions) are reported and skipped — the batch continues.\n" +
			"Ctrl-C stops cleanly after the in-flight session commits.\n\n" +
			"Idempotent: re-running on the same window does nothing once\n" +
			"every session has a summary. The default --limit=100 caps a\n" +
			"runaway fill on a wide window; loosen as needed.\n\n" +
			"Requires ANTHROPIC_API_KEY (or the configured api_key_command).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)
			newClient := func() (llm.Client, error) {
				return llm.FromConfig(cmd.Context(), llmCfg)
			}

			sinceMs := time.Now().Add(-since).UnixMilli()
			rows, err := store.LoadSessionsMissingSummary(cmd.Context(), s.DB(),
				sinceMs, store.SessionFilter{Cwd: cwd, Agent: agent}, limit)
			if err != nil {
				return fmt.Errorf("summaries fill: %w", err)
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no sessions missing a summary in the window")
				return nil
			}
			// Header so the user sees what's happening before the
			// first LLM call's network round-trip lands. Includes
			// the resolved model + window so an unexpected default
			// doesn't take a whole batch's worth of API calls to
			// notice. JSON mode skips it: pipelines want a clean
			// JSON array, not prose preamble.
			if format != FormatJSON {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"filling %d sessions  window=%s  model=%s  provider=%s\n",
					len(rows), humanDuration(since),
					resolveModelLabel(llmCfg, model),
					providerLabel(llmCfg))
			}
			return runSummariesFill(cmd.Context(), s, newClient,
				rows, model, cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout),
				format, cmd.OutOrStdout())
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultSummariesWindow,
		"only consider sessions whose ended_at is within this window (e.g. 24h, 7d)")
	cmd.Flags().IntVar(&limit, "limit", 100, "max sessions to summarize in this run")
	cmd.Flags().StringVar(&cwd, "cwd", "", "filter by exact cwd")
	cmd.Flags().StringVar(&agent, "agent", "", "filter by source_agent (claude-code | codex)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// fillStatus is one row of the streaming output `summaries fill`
// emits. status is "summarized" | "failed" | "skipped" so callers
// piping to jq can branch on a stable string.
type fillStatus struct {
	SessionID    string `json:"session_id"`
	Status       string `json:"status"`
	Topic        string `json:"topic,omitempty"`
	Error        string `json:"error,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
}

// runSummariesFill drives the per-session loop. Streams a one-line
// "[i/N] <id> starting…" before each LLM call AND a result line
// after, so the user sees progress immediately even when the first
// summarize takes 10s. JSON mode accumulates and emits one array
// at the end.
//
// Per-session timeouts come from the same config knob `summarize`
// uses; ctx cancellation propagates so Ctrl-C stops between
// sessions cleanly.
func runSummariesFill(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	rows []store.SessionDigestRow,
	model string,
	perCallTimeout time.Duration,
	format OutputFormat,
	out io.Writer,
) error {
	results := make([]fillStatus, 0, len(rows))
	var filled, failed, skipped int
	total := len(rows)

	defer func() {
		// Always emit json if requested, even on early-exit so the
		// caller's pipeline doesn't see a half-built stream.
		if format == FormatJSON {
			_ = writeJSONFillResults(out, results)
			return
		}
		var totalIn, totalOut int64
		for _, r := range results {
			totalIn += r.InputTokens
			totalOut += r.OutputTokens
		}
		_, _ = fmt.Fprintf(out, "\nfilled: %d  failed: %d  skipped: %d  total: %d",
			filled, failed, skipped, len(results))
		if totalIn > 0 || totalOut > 0 {
			_, _ = fmt.Fprintf(out, "  tokens: %din / %dout", totalIn, totalOut)
		}
		_, _ = fmt.Fprintln(out)
	}()

	// Use a Flusher when the writer is one (os.Stdout buffered to
	// stderr, etc.) so the "starting" line surfaces *before* the
	// LLM call rather than batching with the result line. The
	// type assertion is cheap; missing Flush just means the writer
	// already flushes per-Write, which is the os.Stdout default.
	flusher, _ := out.(interface{ Flush() error })

	for i, row := range rows {
		// Honor ctx cancellation between sessions so Ctrl-C
		// doesn't kill an in-flight summarize mid-write.
		if err := ctx.Err(); err != nil {
			return err
		}

		idx := i + 1
		if format != FormatJSON {
			_, _ = fmt.Fprintf(out, "[%d/%d] %s starting...\n",
				idx, total, shortSessionID(row.ID))
			if flusher != nil {
				_ = flusher.Flush()
			}
		}

		callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
		start := time.Now()
		// RunSummarize writes the rendered summary to its out
		// arg; we discard that here because the per-row line
		// is what the user reads. The cached body is also
		// available via `summaries show` after the run.
		_, err := RunSummarize(callCtx, s, newClient, SummarizeOptions{
			SessionID: row.ID,
			Model:     model,
		}, io.Discard)
		elapsed := time.Since(start).Milliseconds()
		cancel()

		st := fillStatus{SessionID: row.ID, DurationMs: elapsed}
		if err != nil {
			st.Status = "failed"
			st.Error = err.Error()
			failed++
		} else {
			st.Status = "summarized"
			// One DB hit covers both topic + token lookup so we
			// don't double-query on the just-written row.
			if r := latestSummaryRow(ctx, s, row.ID); r != nil {
				st.Topic = extractTopic(store.LLMKindSummary, r.Body)
				if r.InputTokens.Valid {
					st.InputTokens = r.InputTokens.Int64
				}
				if r.OutputTokens.Valid {
					st.OutputTokens = r.OutputTokens.Int64
				}
			}
			filled++
		}
		results = append(results, st)
		if format != FormatJSON {
			emitFillStatusLine(out, st, idx, total)
		}
	}
	return nil
}

// emitFillStatusLine prints one human-readable line per session
// as the fill progresses. The "[i/N]" prefix tells the user where
// in the batch they are without having to count their own output;
// the status glyph (✓ / ✗ / ⚠) is constant-width so the topic /
// error column lines up even when the batch hits triple digits.
// On success the tokens consumed by the LLM call land at the end
// of the line ("12345in/678out") so the user sees per-session cost
// shape without having to query the store afterwards.
func emitFillStatusLine(w io.Writer, s fillStatus, idx, total int) {
	short := shortSessionID(s.SessionID)
	prefix := fmt.Sprintf("[%d/%d]", idx, total)
	switch s.Status {
	case "summarized":
		_, _ = fmt.Fprintf(w, "%s %s ✓ summarized   %q  (%dms%s)\n",
			prefix, short, s.Topic, s.DurationMs, formatTokenSuffix(s.InputTokens, s.OutputTokens))
	case "failed":
		_, _ = fmt.Fprintf(w, "%s %s ✗ failed       %s\n",
			prefix, short, s.Error)
	case "skipped":
		_, _ = fmt.Fprintf(w, "%s %s ⚠ skipped      (%s)\n",
			prefix, short, s.Error)
	}
}

// formatTokenSuffix renders ", N_in/N_out" (e.g. "12k in / 700 out"
// shape, with literal counts) when either token count is non-zero.
// Empty when neither side reported usage — some providers omit it
// on cached / partial responses, and we don't fake numbers we don't
// have. Returned with the leading separator so the caller can
// splice it into the line cleanly.
func formatTokenSuffix(in, out int64) string {
	if in == 0 && out == 0 {
		return ""
	}
	return fmt.Sprintf(", %din/%dout", in, out)
}

// writeJSONFillResults emits the accumulated fillStatus slice as
// indented JSON. Separate function so the deferred emitter in
// runSummariesFill can call it without a closure.
func writeJSONFillResults(w io.Writer, results []fillStatus) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(results)
}

// latestSummaryRow returns the most recent kind=summary llm_outputs
// row for a session, or nil when none exists / on a query error.
// Shared between topic + token lookups so each per-session reporter
// only hits the DB once after a successful summarise.
func latestSummaryRow(ctx context.Context, s *store.Store, sessionID string) *store.LLMOutput {
	outs, err := store.LoadLLMOutputsForSession(ctx, s.DB(), sessionID)
	if err != nil {
		return nil
	}
	for _, o := range outs {
		if o.Kind == store.LLMKindSummary {
			row := o
			return &row
		}
	}
	return nil
}

// writeMissingSummaries renders the LoadSessionsMissingSummary
// result. Reuses the `aichronicles sessions` formatters so the
// table layout matches column-for-column — muscle memory carries.
func writeMissingSummaries(w io.Writer, rows []store.SessionDigestRow, format OutputFormat) error {
	if format == FormatJSON {
		return writeMissingSummariesJSON(w, rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no sessions missing a summary in the window")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SESSION\tSTARTED\tENDED\tCWD\tFIRST_PROMPT")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			shortSessionID(r.ID),
			formatTsNullable(r.StartedAtMs),
			formatTsNullable(r.EndedAtMs),
			nullStringOrDash(r.Cwd),
			truncatePrompt(nullStringOrDash(r.FirstPrompt)),
		)
	}
	return tw.Flush()
}

// writeMissingSummariesJSON shapes the rows into the same JSON that
// `aichronicles sessions --format=json` produces, so jq pipelines
// work unchanged.
func writeMissingSummariesJSON(w io.Writer, rows []store.SessionDigestRow) error {
	type out struct {
		ID          string `json:"id"`
		StartedAtMs int64  `json:"started_at_ms,omitempty"`
		EndedAtMs   int64  `json:"ended_at_ms,omitempty"`
		Cwd         string `json:"cwd,omitempty"`
		FirstPrompt string `json:"first_prompt,omitempty"`
	}
	dst := make([]out, 0, len(rows))
	for _, r := range rows {
		o := out{ID: r.ID}
		if r.StartedAtMs.Valid {
			o.StartedAtMs = r.StartedAtMs.Int64
		}
		if r.EndedAtMs.Valid {
			o.EndedAtMs = r.EndedAtMs.Int64
		}
		if r.Cwd.Valid {
			o.Cwd = r.Cwd.String
		}
		if r.FirstPrompt.Valid {
			o.FirstPrompt = r.FirstPrompt.String
		}
		dst = append(dst, o)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(dst)
}

func newSummariesListCmd() *cobra.Command {
	var (
		sessionIn string
		typeIn    string
		limit     int
		dbPath    string
		formatIn  string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent stored LLM outputs",
		Long: "Prints stored llm_outputs rows newest-first. Without flags, it\n" +
			"shows the latest 50 across every session and every output type.\n" +
			"Filter with --session (prefix OK, same rules as `summarize`),\n" +
			"--type (summary | reflect | propose), or both.\n\n" +
			"Topic column is extracted from the stored JSON body when\n" +
			"possible; rows whose body is not parseable as a known type\n" +
			"show `(unparseable)` so the row is still discoverable by id.\n\n" +
			"Pass --format=json for a structured payload suitable for jq.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			filter := store.LLMOutputFilter{Limit: limit}
			if sessionIn != "" {
				sid, err := store.ResolveSessionIDPrefix(cmd.Context(), s.DB(), sessionIn)
				if err != nil {
					return fmt.Errorf("summaries list: %w", err)
				}
				filter.SessionID = sid
			}
			if typeIn != "" {
				k, err := parseOutputKind(typeIn)
				if err != nil {
					return err
				}
				filter.Kind = k
			}

			rows, err := store.LoadLLMOutputs(cmd.Context(), s.DB(), filter)
			if err != nil {
				return fmt.Errorf("summaries list: %w", err)
			}
			return writeSummaries(cmd.OutOrStdout(), rows, format)
		},
	}
	cmd.Flags().StringVar(&sessionIn, "session", "", "filter by session id or unique prefix")
	registerSessionFlagCompletion(cmd)
	cmd.Flags().StringVar(&typeIn, "type", "", "filter by output type (summary | reflect | propose)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows to list (default 50)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newSummariesShowCmd() *cobra.Command {
	var (
		typeIn   string
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "show <session>",
		Short: "Show the most recent stored LLM output for a session",
		Long: "Renders the latest llm_outputs row matching the given session\n" +
			"(prefix OK) and type (default: summary). Pass --format=json to\n" +
			"emit the raw JSON body instead of the human-readable render —\n" +
			"useful for piping into `jq`.\n\n" +
			"Errors with `no output for session …/type …` when the session\n" +
			"exists but has never been summarized/reflected/proposed under\n" +
			"the requested type.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSessionID,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			sid, err := store.ResolveSessionIDPrefix(cmd.Context(), s.DB(), args[0])
			if err != nil {
				return fmt.Errorf("summaries show: %w", err)
			}
			kind, err := parseOutputKind(typeIn)
			if err != nil {
				return err
			}

			rows, err := store.LoadLLMOutputs(cmd.Context(), s.DB(), store.LLMOutputFilter{
				SessionID: sid,
				Kind:      kind,
				Limit:     1,
			})
			if err != nil {
				return fmt.Errorf("summaries show: %w", err)
			}
			if len(rows) == 0 {
				return fmt.Errorf("no %s output for session %s", kind, sid)
			}
			return emitLLMBody(cmd.OutOrStdout(), kind, rows[0].Body, format == FormatJSON)
		},
	}
	cmd.Flags().StringVar(&typeIn, "type", "summary", "output type (summary | reflect | propose)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// parseOutputKind normalizes the --kind flag into a store.LLMOutputKind.
// Accepting the short forms the CLI prints in its listing rather than
// forcing users to type the full lowercase string.
func parseOutputKind(s string) (store.LLMOutputKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "summary":
		return store.LLMKindSummary, nil
	case "reflect", "reflection":
		return store.LLMKindReflect, nil
	case "propose", "proposal":
		return store.LLMKindPropose, nil
	default:
		return "", fmt.Errorf("unknown kind %q (want summary | reflect | propose)", s)
	}
}

// LLMOutputRowJSON is the JSON shape emitted by `summaries list
// --format=json`. The body field carries the stored llm_outputs.body
// verbatim — already JSON, so we round-trip it through json.RawMessage
// to keep the output a single tree rather than embedding a quoted
// string. Topic is the same scannable label the table renders.
type LLMOutputRowJSON struct {
	ID           int64           `json:"id"`
	Kind         string          `json:"kind"`
	SessionID    *string         `json:"session_id"`
	CreatedAtMs  int64           `json:"created_at_ms"`
	Topic        string          `json:"topic"`
	Model        string          `json:"model,omitempty"`
	InputTokens  *int64          `json:"input_tokens,omitempty"`
	OutputTokens *int64          `json:"output_tokens,omitempty"`
	Body         json.RawMessage `json:"body"`
}

// writeSummaries renders rows in either format. Format=table is the
// human-readable tab-aligned table; format=json is a JSON array of
// LLMOutputRowJSON for jq pipelines. Empty result is "(no outputs)"
// in table mode and "[]" in JSON mode.
func writeSummaries(w io.Writer, rows []store.LLMOutput, format OutputFormat) error {
	if format == FormatJSON {
		payload := make([]LLMOutputRowJSON, 0, len(rows))
		for _, r := range rows {
			row := LLMOutputRowJSON{
				ID:           r.ID,
				Kind:         string(r.Kind),
				SessionID:    nullableString(r.SessionID),
				CreatedAtMs:  r.CreatedAtMs,
				Topic:        extractTopic(r.Kind, r.Body),
				Model:        r.Model,
				InputTokens:  nullableInt64(r.InputTokens),
				OutputTokens: nullableInt64(r.OutputTokens),
				Body:         json.RawMessage(r.Body),
			}
			// Body may have been stored as plain text (legacy or
			// unparseable) — wrap as a JSON string so the array still
			// validates instead of erroring on emit.
			if !json.Valid([]byte(r.Body)) {
				escaped, _ := json.Marshal(r.Body)
				row.Body = escaped
			}
			payload = append(payload, row)
		}
		return emitJSON(w, payload)
	}

	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "(no outputs)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tKIND\tSESSION\tWHEN\tTOPIC"); err != nil {
		return err
	}
	for _, r := range rows {
		topic := extractTopic(r.Kind, r.Body)
		sess := "(multi)"
		if r.SessionID.Valid && r.SessionID.String != "" {
			sess = shortSessionID(r.SessionID.String)
		}
		when := formatTimeForUser(r.CreatedAtMs, time.Now())
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			r.ID, r.Kind, sess, when, topic); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// shortSessionID returns the first 8 chars of a full UUID — matches
// the preview `aichronicles sessions` prints, so column alignment
// across commands stays consistent.
func shortSessionID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// extractTopic picks a short, human-meaningful label out of a stored
// body based on kind. Falls back to "(unparseable)" when the body
// doesn't fit the expected schema — survival mode for legacy rows or
// post-schema-bump drift.
func extractTopic(kind store.LLMOutputKind, body string) string {
	const maxLen = 80
	truncate := func(s string) string {
		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) > maxLen {
			return s[:maxLen] + "…"
		}
		return s
	}
	switch kind {
	case store.LLMKindSummary:
		var r struct {
			Topic string `json:"topic"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil || r.Topic == "" {
			return "(unparseable)"
		}
		return truncate(r.Topic)
	case store.LLMKindReflect:
		var r struct {
			WorkflowChange string `json:"workflow_change"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return "(unparseable)"
		}
		if r.WorkflowChange == "" {
			return "(no workflow change suggested)"
		}
		return truncate(r.WorkflowChange)
	case store.LLMKindPropose:
		var r struct {
			Skills []struct {
				Name string `json:"name"`
			} `json:"skills"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return "(unparseable)"
		}
		switch len(r.Skills) {
		case 0:
			return "(no proposals)"
		case 1:
			return truncate(r.Skills[0].Name)
		default:
			// First skill + "(+ N more)" so the row still fits.
			return truncate(fmt.Sprintf("%s (+%d more)", r.Skills[0].Name, len(r.Skills)-1))
		}
	}
	return "(unknown kind)"
}
