package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// defaults mirror reflect: same window/limit semantics, same
// "enough-sessions-for-patterns, recent-enough-to-matter" balance.
const (
	defaultProposeWindow = 7 * 24 * time.Hour
	defaultProposeLimit  = 25
)

func newProposeCmd() *cobra.Command {
	var (
		since  time.Duration
		limit  int
		model  string
		force  bool
		dbPath string
	)
	cmd := &cobra.Command{
		Use:   "propose",
		Short: "LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions",
		Long: "Reads recent sessions (same window semantics as `reflect`) and\n" +
			"asks the LLM to propose concrete reusable capabilities: new\n" +
			"slash-commands, CLAUDE.md rules, and scripts to pre-build. The\n" +
			"system prompt forbids generic advice — every suggestion must cite\n" +
			"at least one session as evidence.\n\n" +
			"Cached on prompt_hash in llm_outputs with kind=propose. Use\n" +
			"--force to re-call.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved := dbPath
			if resolved == "" {
				p, err := paths.StorePath()
				if err != nil {
					return err
				}
				resolved = p
			}
			s, err := store.Open(resolved)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			ctx, cancel := context.WithTimeout(cmd.Context(), metaLLMTimeout)
			defer cancel()

			_, err = RunPropose(ctx, s, func() (llm.Client, error) { return llm.FromEnv() },
				ProposeOptions{Since: since, Limit: limit, Model: model, Force: force},
				cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().DurationVar(&since, "since", defaultProposeWindow, "only consider sessions whose ended_at is within this window")
	cmd.Flags().IntVar(&limit, "limit", defaultProposeLimit, "max sessions to feed the LLM, newest first")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
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

	digests := digestsFromRows(rows)
	built, err := prompts.BuildPropose(digests)
	if err != nil {
		return 0, fmt.Errorf("propose: build prompt: %w", err)
	}

	return runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:   store.LLMKindPropose,
		hash:   built.Hash,
		req:    built.Request,
		model:  opts.Model,
		force:  opts.Force,
		output: out,
	})
}
