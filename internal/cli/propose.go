package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// defaultProposeWindow defaults to a rolling 7 days: long enough
// that a Monday morning "what should I tackle next" still pulls
// in last week's loose threads, short enough that the prompt
// doesn't drown in stale stuff that's already been resolved.
// Override with --since for a tighter or wider net (e.g.
// --since 24h for "today only").
//
// defaultProposeLimit caps how many sessions feed the prompt
// before we send it to the model — same balance as reflect:
// enough sessions to expose patterns, few enough to fit a
// reasonable prompt budget.
const (
	defaultProposeWindow = 7 * 24 * time.Hour
	defaultProposeLimit  = 25
)

func newProposeCmd() *cobra.Command {
	var (
		since    time.Duration
		limit    int
		model    string
		force    bool
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "propose",
		Short: "LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions",
		Long: "Reads recent sessions (same window semantics as `reflect`) and,\n" +
			"via the record_proposal tool, asks the LLM to propose concrete\n" +
			"reusable capabilities: new slash-commands, CLAUDE.md rules, and\n" +
			"scripts to pre-build. The system prompt forbids generic advice —\n" +
			"every suggestion must cite at least one session as evidence.\n\n" +
			"Cached on prompt_hash in llm_outputs with kind=propose. Use\n" +
			"--force to re-call. Use --format=json to emit the raw JSON body.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			resolved, err := paths.ResolveStorePath(dbPath)
			if err != nil {
				return err
			}
			s, err := store.Open(resolved)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := llmConfigFromFile(cfg.LLM)

			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()

			_, err = RunPropose(ctx, s,
				func() (llm.Client, error) {
					return llm.FromConfig(ctx, llmCfg)
				},
				ProposeOptions{Since: since, Limit: limit, Model: model, Force: force, JSON: format == FormatJSON},
				cmd.OutOrStdout())
			return err
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultProposeWindow, "only consider sessions whose ended_at is within this window (e.g. 24h, 7d)")
	cmd.Flags().IntVar(&limit, "limit", defaultProposeLimit, "max sessions to feed the LLM, newest first")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	cmd.AddCommand(newProposeApplyCmd())
	cmd.AddCommand(newProposeListCmd())
	return cmd
}

// ProposeOptions drives RunPropose. Shape mirrors ReflectOptions;
// keeping them separate types rather than sharing one avoids
// surprising coupling if one feature grows a flag the other doesn't
// want.
type ProposeOptions struct {
	Since time.Duration
	Limit int
	Model string
	Force bool
	JSON  bool
}

// RunPropose orchestrates the proposal path. Same cache + lazy-client
// + clean-on-failure discipline as RunReflect (via runCachedLLM).
func RunPropose(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts ProposeOptions,
	out io.Writer,
) (int64, error) {
	window := opts.Since
	if window <= 0 {
		window = defaultProposeWindow
	}

	sinceMs := time.Now().Add(-window).UnixMilli()
	rows, err := store.LoadRecentSessionDigests(ctx, s.DB(), sinceMs, opts.Limit)
	if err != nil {
		return 0, fmt.Errorf("propose: load sessions: %w", err)
	}
	if len(rows) == 0 {
		return 0, errors.New("propose: no sessions in the requested window")
	}

	digests, err := digestsFromRowsWithLinks(ctx, s, rows)
	if err != nil {
		return 0, fmt.Errorf("propose: enrich digests: %w", err)
	}

	// Skill-aware enrichment: list every SKILL.md the user has on
	// disk (global + project-local for each session's start cwd) so
	// the LLM doesn't repropose what's already installed, plus the
	// per-skill invocation counts so it knows which ones the user
	// actively uses. Discovery errors are non-fatal — propose runs
	// without the enrichment rather than refusing to proceed.
	installed, err := skills.CollectInstalled(ctx, s.DB(), sinceMs)
	if err != nil {
		slog.Warn("propose: skipping installed-skills enrichment", "err", err)
	}
	invoked, err := skills.LoadInvoked(ctx, s.DB(), sinceMs)
	if err != nil {
		slog.Warn("propose: skipping invoked-skills enrichment", "err", err)
	}

	built, err := prompts.BuildPropose(prompts.ProposeInputs{
		Digests:         digests,
		InstalledSkills: installed,
		InvokedSkills:   invoked,
	})
	if err == nil && len(built.Patterns) > 0 {
		slog.Info("propose: egress redaction fired",
			"patterns", strings.Join(built.Patterns, ","))
	}
	if err != nil {
		return 0, fmt.Errorf("propose: build prompt: %w", err)
	}

	return runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:     store.LLMKindPropose,
		toolName: prompts.ToolNameProposal,
		result:   new(prompts.ProposalResult),
		hash:     built.Hash,
		req:      built.Request,
		model:    opts.Model,
		force:    opts.Force,
		jsonRaw:  opts.JSON,
		output:   out,
	})
}
