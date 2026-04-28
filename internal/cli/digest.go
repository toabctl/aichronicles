package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// weeklyDigestSessionLimit caps how many sessions feed the LLM
// per weekly run. Same balance the ad-hoc `reflect` command
// uses — enough to expose patterns, few enough to fit a
// reasonable prompt budget.
const weeklyDigestSessionLimit = 25

func newDigestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Periodic LLM-driven digests stored as queryable artefacts",
		Long: "Runs reflect over a fixed time window and writes the result\n" +
			"into llm_outputs as a tagged artefact (kind=reflect_weekly).\n" +
			"Unlike ad-hoc `reflect`, the body is wrapped with\n" +
			"period_start/period_end metadata so the result is queryable\n" +
			"as a timeline of past weeks (via `digest list`).\n\n" +
			"Designed to be cron-friendly: rerunning for the same week is\n" +
			"a cache hit on prompt_hash; --force re-calls the LLM.",
	}
	cmd.AddCommand(newDigestWeeklyCmd())
	cmd.AddCommand(newDigestListCmd())
	return cmd
}

func newDigestWeeklyCmd() *cobra.Command {
	var (
		weekOf   string
		force    bool
		dbPath   string
		formatIn string
		model    string
	)
	cmd := &cobra.Command{
		Use:   "weekly",
		Short: "Generate a weekly reflect digest, persisted with kind=reflect_weekly",
		Long: "Computes the previous completed Monday-00:00-UTC →\n" +
			"Monday-00:00-UTC week and runs reflect over the sessions in\n" +
			"that window. Override the period with --week-of <YYYY-MM-DD>\n" +
			"to target a different Monday (the date you pass is anchored\n" +
			"to that week's Monday).\n\n" +
			"Re-running the same week is a cache hit (the period dates are\n" +
			"in the prompt's user message, so the prompt_hash naturally\n" +
			"differs across weeks but stays stable for a given week).\n" +
			"Pass --force to re-call the LLM.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
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

			start, end, err := resolveWeekBounds(weekOf, time.Now().UTC())
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()

			_, err = RunDigestWeekly(ctx, s,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				DigestWeeklyOptions{
					PeriodStart: start,
					PeriodEnd:   end,
					Limit:       weeklyDigestSessionLimit,
					Force:       force,
					Model:       model,
					JSON:        format == FormatJSON,
				},
				cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&weekOf, "week-of", "",
		"target a specific Monday (YYYY-MM-DD); default is the previous completed week")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newDigestListCmd() *cobra.Command {
	var (
		dbPath   string
		formatIn string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List past weekly digest artefacts (kind=reflect_weekly)",
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

			rows, err := store.LoadLLMOutputs(cmd.Context(), s.DB(), store.LLMOutputFilter{
				Kind:  store.LLMKindReflectWeekly,
				Limit: limit,
			})
			if err != nil {
				return fmt.Errorf("list digests: %w", err)
			}
			return renderDigestList(cmd.OutOrStdout(), rows, format)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "max digests to list, newest first")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// DigestWeeklyOptions drives RunDigestWeekly.
type DigestWeeklyOptions struct {
	PeriodStart time.Time // Monday 00:00 UTC inclusive
	PeriodEnd   time.Time // next Monday 00:00 UTC exclusive
	Limit       int
	Force       bool
	Model       string
	JSON        bool
}

// WeeklyDigestEnvelope is a re-export of prompts.WeeklyDigestEnvelope
// kept here for backwards compatibility with the in-package callers.
// New callers (especially anything outside internal/cli) should
// reference the canonical type in pkg/llm/prompts directly so that
// the cli package doesn't accumulate import-cycle pressure when
// other layers (web, mcp) need to read the persisted body shape.
type WeeklyDigestEnvelope = prompts.WeeklyDigestEnvelope

// RunDigestWeekly is the reusable entry point: builds the reflect
// prompt over [PeriodStart, PeriodEnd), calls the LLM through the
// shared cache, wraps the result with period metadata, and
// persists it under kind=reflect_weekly.
func RunDigestWeekly(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts DigestWeeklyOptions,
	out io.Writer,
) (int64, error) {
	if !opts.PeriodStart.Before(opts.PeriodEnd) {
		return 0, errors.New("digest weekly: PeriodStart must be before PeriodEnd")
	}

	rows, err := store.LoadRecentSessionDigests(ctx, s.DB(),
		opts.PeriodStart.UnixMilli(), opts.Limit)
	if err != nil {
		return 0, fmt.Errorf("digest weekly: load sessions: %w", err)
	}
	// Filter to sessions that ended INSIDE the window. The store
	// query takes a since-cutoff but no upper bound; we apply
	// the upper bound here to keep the prompt anchored to the
	// requested week.
	rows = filterRowsBefore(rows, opts.PeriodEnd.UnixMilli())
	if len(rows) == 0 {
		return 0, fmt.Errorf("digest weekly: no sessions in week of %s",
			opts.PeriodStart.Format("2006-01-02"))
	}

	digests, err := digestsFromRowsWithLinks(ctx, s, rows)
	if err != nil {
		return 0, fmt.Errorf("digest weekly: enrich digests: %w", err)
	}
	if len(digests) == 0 {
		return 0, fmt.Errorf("digest weekly: no summarised sessions in week of %s",
			opts.PeriodStart.Format("2006-01-02"))
	}

	built, err := prompts.BuildReflect(digests, opts.PeriodEnd.Sub(opts.PeriodStart))
	if err != nil {
		return 0, fmt.Errorf("digest weekly: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("digest weekly: egress redaction fired",
			"patterns", strings.Join(built.Patterns, ","))
	}

	id, err := runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:     store.LLMKindReflectWeekly,
		toolName: prompts.ToolNameReflection,
		result:   new(prompts.ReflectionResult),
		hash:     digestHashFor(built.Hash, opts.PeriodStart, opts.PeriodEnd),
		req:      built.Request,
		model:    opts.Model,
		force:    opts.Force,
		// Suppress the inner Reflect rendering — we render our
		// own envelope-wrapped output below.
		jsonRaw: true,
		output:  io.Discard,
	})
	if err != nil {
		return id, err
	}

	// Re-load the cached row so we can wrap the persisted body
	// (the inner reflect ReflectionResult JSON) with period
	// metadata for the user-facing render. The persisted body
	// stays the bare ReflectionResult so cache hits don't
	// double-wrap; the WeeklyDigestEnvelope shape is computed
	// at read time from period boundaries we stored alongside
	// the row's prompt hash.
	stored, err := store.LoadLLMOutputByID(ctx, s.DB(), id)
	if err != nil || stored == nil {
		return id, fmt.Errorf("digest weekly: re-read persisted body: %w", err)
	}

	var inner prompts.ReflectionResult
	if err := json.Unmarshal([]byte(stored.Body), &inner); err != nil {
		return id, fmt.Errorf("digest weekly: parse stored body: %w", err)
	}
	envelope := WeeklyDigestEnvelope{
		PeriodStart: opts.PeriodStart.Format(time.RFC3339),
		PeriodEnd:   opts.PeriodEnd.Format(time.RFC3339),
		Result:      &inner,
	}
	if opts.JSON {
		return id, emitJSON(out, envelope)
	}
	return id, renderWeeklyDigest(out, envelope)
}

// resolveWeekBounds returns the [Monday 00:00 UTC, next Monday
// 00:00 UTC) interval for the previous completed week. The
// canonical trigger is the Mon 06:00 UTC cron: at that point the
// just-completed Mon-Sun week is the most recent finished period
// and that's what we digest. weekOfArg, when non-empty, targets
// a specific Monday by YYYY-MM-DD — any date in that week works;
// we snap to the Monday that starts it.
//
// For ad-hoc runs in the middle of a week, this still returns
// the last fully-elapsed week (more conservative than "this week
// so far" — the data is complete and the digest is stable).
func resolveWeekBounds(weekOfArg string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	var anchor time.Time
	if weekOfArg == "" {
		// mondayOf(now) is this week's Monday; subtract 7 days
		// to get the Monday that started the most recent fully-
		// elapsed week.
		anchor = mondayOf(now).AddDate(0, 0, -7)
	} else {
		t, err := time.Parse("2006-01-02", weekOfArg)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--week-of: %w", err)
		}
		anchor = mondayOf(t.UTC())
	}
	start := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 7)
	return start, end, nil
}

// mondayOf returns the Monday of the ISO week containing t.
func mondayOf(t time.Time) time.Time {
	dow := int(t.Weekday())
	if dow == 0 { // Sunday → previous Monday is 6 days back
		dow = 7
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -(dow - 1))
}

// filterRowsBefore drops every row whose effective end timestamp
// is at or after upperMs. We use COALESCE(ended_at, started_at,
// 0) like the rest of the store — same definition the prune /
// insights / sessions paths use.
func filterRowsBefore(rows []store.SessionDigestRow, upperMs int64) []store.SessionDigestRow {
	out := rows[:0]
	for _, r := range rows {
		end := int64(0)
		if r.EndedAtMs.Valid {
			end = r.EndedAtMs.Int64
		} else if r.StartedAtMs.Valid {
			end = r.StartedAtMs.Int64
		}
		if end > 0 && end < upperMs {
			out = append(out, r)
		}
	}
	return out
}

// digestHashFor mixes the underlying reflect prompt hash with the
// period bounds. Without this, two weeks with overlapping session
// digest content could collide in cache (unlikely but possible);
// with it, the period uniqueness propagates into the cache key.
func digestHashFor(promptHash string, start, end time.Time) string {
	return promptHash + "|" + start.Format(time.RFC3339) + "|" + end.Format(time.RFC3339)
}

// renderWeeklyDigest is the human-readable text format. JSON mode
// goes through emitJSON in RunDigestWeekly directly.
func renderWeeklyDigest(out io.Writer, env WeeklyDigestEnvelope) error {
	var b strings.Builder
	fmt.Fprintf(&b, "weekly digest — %s → %s\n\n", env.PeriodStart, env.PeriodEnd)
	if env.Result == nil {
		_, err := io.WriteString(out, b.String())
		return err
	}
	if len(env.Result.TaskTypes) > 0 {
		fmt.Fprintln(&b, "Task types:")
		for _, t := range env.Result.TaskTypes {
			fmt.Fprintf(&b, "  - %s  [freq=%d]\n", t.Label, t.Frequency)
		}
		b.WriteByte('\n')
	}
	if len(env.Result.Frictions) > 0 {
		fmt.Fprintln(&b, "Frictions:")
		for _, f := range env.Result.Frictions {
			fmt.Fprintf(&b, "  - %s  [freq=%d severity=%s]\n", f.Label, f.Frequency, f.Severity)
		}
		b.WriteByte('\n')
	}
	if env.Result.WorkflowChange != "" {
		fmt.Fprintln(&b, "Workflow change:")
		fmt.Fprintf(&b, "  %s\n", env.Result.WorkflowChange)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// renderDigestList prints a tabular summary of past weekly digest
// rows so the user can see what's in the timeline. JSON mode emits
// the raw envelope shape for jq.
func renderDigestList(out io.Writer, rows []store.LLMOutput, format OutputFormat) error {
	if format == FormatJSON {
		envs := make([]WeeklyDigestEnvelope, 0, len(rows))
		for _, r := range rows {
			env, err := decodeStoredEnvelope(&r)
			if err != nil {
				continue
			}
			envs = append(envs, env)
		}
		return emitJSON(out, envs)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "(no weekly digests yet — run `aichronicles digest weekly` to create one)")
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "weekly digests (%d):\n", len(rows))
	for _, r := range rows {
		var inner prompts.ReflectionResult
		_ = json.Unmarshal([]byte(r.Body), &inner)
		// We don't store period metadata in the row, only in the
		// prompt; recover it from the cached prompt's
		// "Below are %d sessions from %s to %s" header is
		// brittle. For the list view, fall back to the row's
		// created_at as a stable proxy.
		when := time.UnixMilli(r.CreatedAtMs).UTC().Format("2006-01-02")
		change := strings.TrimSpace(inner.WorkflowChange)
		if change == "" {
			change = "(no workflow_change recorded)"
		}
		fmt.Fprintf(&b, "  [id=%d] %s  · %d task types · %d frictions\n      %s\n",
			r.ID, when, len(inner.TaskTypes), len(inner.Frictions),
			truncateRunes(change, 100))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// decodeStoredEnvelope reconstructs a WeeklyDigestEnvelope from a
// stored row. The body holds the bare ReflectionResult; we don't
// have a recorded period start/end so we leave those empty in
// JSON output — the consumer can use created_at_ms instead.
func decodeStoredEnvelope(r *store.LLMOutput) (WeeklyDigestEnvelope, error) {
	var inner prompts.ReflectionResult
	if err := json.Unmarshal([]byte(r.Body), &inner); err != nil {
		return WeeklyDigestEnvelope{}, err
	}
	return WeeklyDigestEnvelope{
		Result: &inner,
	}, nil
}

// truncateRunes is the utility cousin of clipToRunes (no word-
// boundary preference) used for compact list previews.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
