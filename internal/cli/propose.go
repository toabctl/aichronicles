package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// defaults mirror reflect: same window/limit semantics, same
// "enough-sessions-for-patterns, recent-enough-to-matter" balance.
const (
	defaultProposeWindow = 7 * 24 * time.Hour
	defaultProposeLimit  = 25
)

func newProposeCmd() *cobra.Command {
	var (
		since   time.Duration
		limit   int
		model   string
		force   bool
		jsonOut bool
		dbPath  string
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
			"--force to re-call. Use --json to emit the raw JSON body.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				ProposeOptions{Since: since, Limit: limit, Model: model, Force: force, JSON: jsonOut},
				cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().DurationVar(&since, "since", defaultProposeWindow, "only consider sessions whose ended_at is within this window")
	cmd.Flags().IntVar(&limit, "limit", defaultProposeLimit, "max sessions to feed the LLM, newest first")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON body instead of the human-readable render")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
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
	built, err := prompts.BuildPropose(digests)
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
