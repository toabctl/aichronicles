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
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// defaultInductionIdle re-exports store.DefaultInductionIdle as
// the local sweep flag's default so cli and store can't drift.
// 30 minutes is comfortable: long enough that an active conversation
// pausing for a coffee break doesn't trip the sweeper, short enough
// that a finished session gets processed while it's still
// cognitively warm for the user.
const defaultInductionIdle = store.DefaultInductionIdle

// defaultInductionMinEvents drops trivially-short sessions ("user
// typed `q` and bailed") from the sweep. 5 events is one
// user_prompt + one assistant_message + a tool_use round-trip;
// anything below that won't ground a workflow.
const defaultInductionMinEvents = 5

// defaultInductionLimit caps how many sessions one sweep
// processes. With ~50¢/run typical and the session-level cache
// keeping re-runs free, a manual `induction sweep` walking 25
// at a time is the right blast radius.
const defaultInductionLimit = 25

func newInductionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "induction",
		Short: "Online single-session induction (AWM-style auto-skill-extraction)",
		Long: "When a session ends, induction asks the LLM whether the\n" +
			"trajectory contained ONE concrete reusable workflow worth\n" +
			"saving as a Claude Code skill. The bar is high — the model\n" +
			"is told to default to no_skill_found unless it can name a\n" +
			"specific trigger condition the user is likely to hit again.\n\n" +
			"Subcommands:\n" +
			"  sweep — walk recently-ended sessions and induce on each\n" +
			"  run   — induce on one specific session id\n" +
			"  list  — show induction outcomes recorded so far\n",
	}
	cmd.AddCommand(newInductionSweepCmd())
	cmd.AddCommand(newInductionRunCmd())
	cmd.AddCommand(newInductionListCmd())
	return cmd
}

func newInductionSweepCmd() *cobra.Command {
	var (
		idle      time.Duration
		minEvents int
		limit     int
		model     string
		dbPath    string
		skipFacts bool
	)
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Walk recently-ended sessions and induce on each",
		Long: "Selects sessions that (a) have an ended_at_ms older than\n" +
			"--idle, (b) have at least --min-events recorded events,\n" +
			"and (c) haven't been induced before, then runs two\n" +
			"single-session induction passes on each: the unified\n" +
			"induction call (extracts skill AND/OR abstract workflow\n" +
			"from one prompt) and a separate semantic-fact induction.\n" +
			"Idempotent — re-running skips sessions whose LLM rows\n" +
			"already exist.\n\n" +
			"Designed to be triggered from a systemd timer / cron job\n" +
			"or by the daemon's resident sweeper. The CLI prints a\n" +
			"per-session line so the operator can tell which session\n" +
			"yielded a skill, a workflow, both, or nothing.\n\n" +
			"Pass --skip-facts to suppress the facts induction call,\n" +
			"halving per-candidate spend (useful for cost-bounded\n" +
			"cron schedules).\n\n" +
			"Requires " + llm.APIKeyEnv + ".",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()

			return RunInductionSweep(ctx, s,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				InductionSweepOptions{
					Idle:      idle,
					MinEvents: minEvents,
					Limit:     limit,
					Model:     model,
					SkipFacts: skipFacts,
				}, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addFlexDurationFlag(cmd, &idle, "idle", defaultInductionIdle,
		"only consider sessions with no events for this long (e.g. 15m, 1h)")
	cmd.Flags().IntVar(&minEvents, "min-events", defaultInductionMinEvents,
		"skip sessions with fewer than this many events")
	cmd.Flags().IntVar(&limit, "limit", defaultInductionLimit,
		"max sessions to process in one sweep")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().BoolVar(&skipFacts, "skip-facts", false,
		"skip the per-candidate semantic-facts induction (saves one LLM call per candidate)")
	return cmd
}

func newInductionRunCmd() *cobra.Command {
	var (
		session  string
		model    string
		force    bool
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "run --session <id>",
		Short: "Induce on one specific session id",
		Long: "Same prompt and persistence as `induction sweep`, but for a\n" +
			"single session you name explicitly. Useful for replaying or\n" +
			"force-recomputing — pair with --force to bypass the cache.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if session == "" {
				return errors.New("--session <id> is required")
			}
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
			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout))
			defer cancel()

			_, err = RunInductionForSession(ctx, s,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				InductionRunOptions{
					SessionID: session,
					Model:     model,
					Force:     force,
					JSON:      format == FormatJSON,
				}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id (full or unique prefix) to induce on")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the cache and re-call the LLM")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newInductionListCmd() *cobra.Command {
	var (
		dbPath   string
		limit    int
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show recent induction runs (proposed skills, no_skill_found verdicts)",
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
				Kind:  store.LLMKindInduction,
				Limit: limit,
			})
			if err != nil {
				return fmt.Errorf("load induction rows: %w", err)
			}
			return renderInductionList(cmd.OutOrStdout(), rows, format)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to render, newest first")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// InductionSweepOptions drives RunInductionSweep.
type InductionSweepOptions struct {
	Idle      time.Duration
	MinEvents int
	Limit     int
	Model     string

	// SkipFacts, when true, suppresses the per-candidate
	// facts-induction LLM call that the sweep otherwise fires
	// alongside the (skill+workflow merged) induction call.
	// Default (SkipFacts=false) is "auto-extract every memory
	// type from a settled session" — 2 LLM calls per candidate.
	// Set true to halve the per-tick spend at the cost of an
	// empty semantic-facts table.
	SkipFacts bool
}

// RunInductionSweep walks idle sessions and runs RunInductionForSession
// on each. Errors on any one session are logged but don't abort the
// sweep — one bad session shouldn't block the rest of the queue.
//
// Returns nil iff every candidate either (a) produced an induction
// row or (b) was already cached. A wrapped error returns when no
// candidates exist (typical case for a fresh DB) so a cron-driven
// caller can detect "nothing to do" cleanly.
func RunInductionSweep(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts InductionSweepOptions,
	out, errOut io.Writer,
) error {
	idle := opts.Idle
	if idle <= 0 {
		idle = defaultInductionIdle
	}
	minEvents := opts.MinEvents
	if minEvents <= 0 {
		minEvents = defaultInductionMinEvents
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultInductionLimit
	}

	candidates, err := store.LoadInductionCandidates(ctx, s.DB(),
		time.Now().UnixMilli(), idle.Milliseconds(), minEvents, limit)
	if err != nil {
		return fmt.Errorf("load induction candidates: %w", err)
	}
	_, _ = fmt.Fprintf(errOut,
		"induction sweep: idle=%s  min_events=%d  candidates=%d\n",
		humanDuration(idle), minEvents, len(candidates))
	if len(candidates) == 0 {
		_, _ = fmt.Fprintf(out, "induction sweep: nothing to do (no idle, un-induced sessions in window)\n")
		return nil
	}

	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := RunInductionForSession(ctx, s, newClient, InductionRunOptions{
			SessionID: c.ID,
			Model:     opts.Model,
		}, out)
		if err != nil {
			// One session's failure shouldn't kill the sweep — log
			// it and continue. The next sweep retries this session
			// (no induction row was written).
			slog.Warn("induction sweep: session failed",
				"session_id", c.ID, "err", err)
			_, _ = fmt.Fprintf(errOut,
				"  ✗ %s: %v\n", c.ID[:8], err)
		}

		// Auto-extract semantic facts from the same candidate when
		// not opted out. Workflow extraction happens INLINE inside
		// RunInductionForSession (Round 8: skill + workflow share
		// one LLM call), so there's no separate workflow path here.
		// Facts is the one auxiliary call that remains.
		if !opts.SkipFacts {
			if _, ferr := RunFactsForSession(ctx, s, newClient, FactsRunOptions{
				SessionID: c.ID,
				Model:     opts.Model,
			}, io.Discard); ferr != nil {
				slog.Warn("induction sweep: facts failed",
					"session_id", c.ID, "err", ferr)
				_, _ = fmt.Fprintf(errOut,
					"  ✗ facts %s: %v\n", c.ID[:8], ferr)
			}
		}
	}
	return nil
}

// InductionRunOptions drives RunInductionForSession.
type InductionRunOptions struct {
	SessionID string
	Model     string
	Force     bool
	JSON      bool
}

// RunInductionForSession does the single-session induction: build
// digest from session row + summary + extractions, build the
// induction prompt, hit cache or call LLM, persist with
// kind='induction', render result.
//
// Returns the llm_outputs row id on cache hit OR fresh write.
//
// The session needs to have been summarized — without a summary
// the digest is too thin to ground a useful induction. Returns a
// clear error in that case rather than running on a stub digest.
func RunInductionForSession(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts InductionRunOptions,
	out io.Writer,
) (int64, error) {
	sessionID, err := store.ResolveSessionIDPrefix(ctx, s.DB(), opts.SessionID)
	if err != nil {
		return 0, fmt.Errorf("induction: %w", err)
	}

	digestRow, err := store.LoadSessionDigest(ctx, s.DB(), sessionID)
	if err != nil {
		return 0, fmt.Errorf("induction: load digest: %w", err)
	}
	if digestRow == nil {
		return 0, fmt.Errorf("induction: session %s not found", sessionID)
	}
	if !digestRow.LatestSummary.Valid || strings.TrimSpace(digestRow.LatestSummary.String) == "" {
		return 0, fmt.Errorf("induction: session %s has no summary — run `aichronicles summarize %s` first",
			sessionID, sessionID)
	}

	digest := prompts.SessionDigest{
		ID:      digestRow.ID,
		Summary: digestRow.LatestSummary.String,
	}
	if digestRow.StartedAtMs.Valid {
		digest.StartedAtMs = digestRow.StartedAtMs.Int64
	}
	if digestRow.EndedAtMs.Valid {
		digest.EndedAtMs = digestRow.EndedAtMs.Int64
	}
	if digestRow.Cwd.Valid {
		digest.Cwd = digestRow.Cwd.String
	}
	if digestRow.FirstPrompt.Valid {
		digest.FirstPrompt = digestRow.FirstPrompt.String
	}
	urls, err := store.LoadExtractionsForSession(ctx, s.DB(), sessionID, "url")
	if err != nil {
		return 0, fmt.Errorf("induction: load urls: %w", err)
	}
	if len(urls) > 0 {
		digest.Links = make([]string, len(urls))
		for i, u := range urls {
			digest.Links[i] = u.Value
		}
	}
	shells, err := store.LoadExtractionsForSession(ctx, s.DB(), sessionID, "shell_command")
	if err != nil {
		return 0, fmt.Errorf("induction: load shell_command: %w", err)
	}
	if len(shells) > 0 {
		digest.ShellCommands = make([]string, len(shells))
		for i, sc := range shells {
			digest.ShellCommands[i] = sc.Value
		}
	}

	// Outcome enrichment: fill the cached row or compute one. Lets
	// the induction prompt apply its outcome-aware bias (failure_likely
	// → default to no_skill_found unless the failure itself reveals a
	// reusable trigger). Best-effort.
	if outcome, oerr := store.EnsureSessionOutcome(ctx, s.DB(), sessionID); oerr != nil {
		slog.Warn("induction: skipping outcome cue", "session", sessionID, "err", oerr)
	} else {
		digest.Outcome = outcome
	}

	// Installed-skills enrichment so the induction prompt won't
	// repropose a skill that already exists. Best-effort: a
	// failure here downgrades the prompt rather than blocking
	// the run.
	installed, ierr := skills.CollectInstalled(ctx, s.DB(),
		time.Now().Add(-30*24*time.Hour).UnixMilli())
	if ierr != nil {
		slog.Warn("induction: skipping installed-skills enrichment", "err", ierr)
	}

	built, err := prompts.BuildInduce(prompts.InduceFromSessionInputs{
		Digest:          digest,
		InstalledSkills: installed,
	})
	if err != nil {
		return 0, fmt.Errorf("induction: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("induction: egress redaction fired",
			"session_id", sessionID,
			"patterns", strings.Join(built.Patterns, ","))
	}

	id, err := runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:      store.LLMKindInduction,
		toolName:  prompts.ToolNameInduction,
		result:    new(prompts.InductionResult),
		hash:      built.Hash,
		req:       built.Request,
		model:     opts.Model,
		force:     opts.Force,
		jsonRaw:   opts.JSON,
		output:    io.Discard, // we render below
		sessionID: sessionID,
	})
	if err != nil {
		return 0, err
	}

	row, err := store.LoadLLMOutputByID(ctx, s.DB(), id)
	if err != nil || row == nil {
		return id, fmt.Errorf("induction: load persisted row: %w", err)
	}
	var result prompts.InductionResult
	if err := json.Unmarshal([]byte(row.Body), &result); err != nil {
		return id, fmt.Errorf("induction: parse persisted body: %w", err)
	}
	// Lifecycle tracking: if the LLM proposed a skill, record it in
	// proposed_skills so a later `propose apply` can attribute the
	// resulting SKILL.md back to this induction run, and so the
	// skill survives in the proposed-skills index even if the user
	// never applies it (the abandonment-rate signal). Best-effort —
	// see recordProposedSkillsFromProposal.
	if result.Skill != nil && result.Skill.Name != "" {
		if rerr := store.RecordProposedSkill(ctx, s.DB(), id, result.Skill.Name, row.CreatedAtMs); rerr != nil {
			slog.Warn("induction: failed to record proposed skill",
				"llm_output_id", id, "skill", result.Skill.Name, "err", rerr)
		}
	}
	if opts.JSON {
		_, _ = io.WriteString(out, row.Body)
		if !strings.HasSuffix(row.Body, "\n") {
			_, _ = io.WriteString(out, "\n")
		}
		return id, nil
	}
	renderInductionResult(out, sessionID, &result)
	return id, nil
}

// renderInductionResult writes a one-screen summary of the
// induction outcome. Three branches: nothing emitted (most
// common), a skill emitted, a workflow emitted, or both. The
// skill / workflow paths are independent — both can fire on the
// same session when the LLM judges both artefacts grounded.
func renderInductionResult(out io.Writer, sessionID string, r *prompts.InductionResult) {
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	if r.Skill == nil && r.Workflow == nil {
		_, _ = fmt.Fprintf(out, "induction: ✓ %s — nothing reusable\n", short)
		if r.Rationale != "" {
			_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
		}
		return
	}
	if r.Skill != nil {
		sk := r.Skill
		_, _ = fmt.Fprintf(out, "induction: ✓ %s — skill %q\n", short, sk.Name)
		if sk.WhenToUse != "" {
			_, _ = fmt.Fprintf(out, "  when_to_use: %s\n", sk.WhenToUse)
		}
		_, _ = fmt.Fprintf(out, "  apply: aichronicles propose apply --skill %s --output-id <id>\n", sk.Name)
	}
	if r.Workflow != nil {
		wf := r.Workflow
		_, _ = fmt.Fprintf(out, "induction: ✓ %s — workflow %q\n", short, wf.TaskShape)
		for i, step := range wf.Procedure {
			_, _ = fmt.Fprintf(out, "    %d. %s\n", i+1, step.Action)
		}
	}
	if r.Rationale != "" {
		_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
	}
}

// renderInductionList renders the induction history — one row per
// induction llm_outputs row, newest first. Topic is the proposed
// skill name OR "(no skill)" when the verdict was negative.
func renderInductionList(out io.Writer, rows []store.LLMOutput, format OutputFormat) error {
	if format == FormatJSON {
		// Round-trip the raw rows so a JSON consumer gets the full
		// body and can branch on the skill/workflow fields themselves.
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "no induction runs recorded yet — try `aichronicles induction sweep`\n")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s  %-19s  %s\n", "session", "when", "outcome")
	fmt.Fprintln(&b, strings.Repeat("-", 80))
	for _, r := range rows {
		var result prompts.InductionResult
		outcome := "(unparseable body)"
		if jerr := json.Unmarshal([]byte(r.Body), &result); jerr == nil {
			outcome = formatInductionOutcome(&result)
		}
		sessShort := "(none)"
		if r.SessionID.Valid && len(r.SessionID.String) >= 8 {
			sessShort = r.SessionID.String[:8]
		}
		when := time.UnixMilli(r.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC")
		fmt.Fprintf(&b, "%-8s  %-19s  %s\n", sessShort, when, outcome)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// formatInductionOutcome renders one cell for the induction-list
// table. Three independent shapes (skill / workflow / both / neither)
// each get their own short string.
func formatInductionOutcome(r *prompts.InductionResult) string {
	if r.Skill == nil && r.Workflow == nil {
		return "nothing — " + r.Rationale
	}
	parts := []string{}
	if r.Skill != nil {
		s := "skill: " + r.Skill.Name
		if r.Skill.WhenToUse != "" {
			s += " — " + truncateForList(r.Skill.WhenToUse, 50)
		}
		parts = append(parts, s)
	}
	if r.Workflow != nil {
		w := "workflow: " + truncateForList(r.Workflow.TaskShape, 50)
		parts = append(parts, w)
	}
	return strings.Join(parts, "; ")
}

// truncateForList trims `s` to fit a list cell. Suffix with `…`
// when truncated so the reader knows there's more.
func truncateForList(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
