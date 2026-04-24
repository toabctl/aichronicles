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

// metaLLMTimeout covers both reflect and propose: same network
// pattern, slightly larger prompts than summarize.
const metaLLMTimeout = 5 * time.Minute

// defaultReflectWindow is what `--since` falls back to when unset.
// A week strikes a balance between "enough sessions to see patterns"
// and "recent enough to still matter."
const defaultReflectWindow = 7 * 24 * time.Hour

// defaultReflectLimit caps how many sessions reflect feeds the LLM
// when the user hasn't specified. Prevents surprise token bills on
// dense weeks.
const defaultReflectLimit = 25

func newReflectCmd() *cobra.Command {
	var (
		since  time.Duration
		limit  int
		model  string
		force  bool
		dbPath string
	)
	cmd := &cobra.Command{
		Use:   "reflect",
		Short: "LLM-derived meta-analysis of recent sessions",
		Long: "Looks at sessions that ended within --since and asks the LLM to\n" +
			"identify recurring task types, recurring sources of friction, and\n" +
			"one workflow change worth trying. Existing per-session summaries\n" +
			"(from `aichronicles summarize`) are preferred to raw first\n" +
			"prompts — cheaper tokens, denser signal.\n\n" +
			"Cached like summarize: same digest list = same prompt_hash = same\n" +
			"cached body. Use --force to re-call.\n\n" +
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

			_, err = RunReflect(ctx, s, func() (llm.Client, error) { return llm.FromEnv() },
				ReflectOptions{Since: since, Limit: limit, Model: model, Force: force},
				cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().DurationVar(&since, "since", defaultReflectWindow, "only consider sessions whose ended_at is within this window (e.g. 168h)")
	cmd.Flags().IntVar(&limit, "limit", defaultReflectLimit, "max sessions to feed the LLM, newest first")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// ReflectOptions drives RunReflect.
type ReflectOptions struct {
	Since time.Duration
	Limit int
	Model string
	Force bool
}

// RunReflect orchestrates the meta-analysis path. Same cache-first /
// lazy-client / LLM-errors-are-clean discipline as RunSummarize.
func RunReflect(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts ReflectOptions,
	out io.Writer,
) (int64, error) {
	window := opts.Since
	if window <= 0 {
		window = defaultReflectWindow
	}

	sinceMs := time.Now().Add(-window).UnixMilli()
	rows, err := store.LoadRecentSessionDigests(s.DB(), sinceMs, opts.Limit)
	if err != nil {
		return 0, fmt.Errorf("reflect: load sessions: %w", err)
	}
	if len(rows) == 0 {
		return 0, errors.New("reflect: no sessions in the requested window")
	}

	digests := digestsFromRows(rows)
	built, err := prompts.BuildReflect(digests, window)
	if err != nil {
		return 0, fmt.Errorf("reflect: build prompt: %w", err)
	}

	return runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:   store.LLMKindReflect,
		hash:   built.Hash,
		req:    built.Request,
		model:  opts.Model,
		force:  opts.Force,
		output: out,
	})
}

// digestsFromRows converts the DB-facing digest rows into the
// prompt-facing shape. NULL summary → empty string (BuildReflect
// treats it as "use first_prompt").
func digestsFromRows(rows []store.SessionDigestRow) []prompts.SessionDigest {
	out := make([]prompts.SessionDigest, 0, len(rows))
	for _, r := range rows {
		d := prompts.SessionDigest{ID: r.ID}
		if r.StartedAtMs.Valid {
			d.StartedAtMs = r.StartedAtMs.Int64
		}
		if r.EndedAtMs.Valid {
			d.EndedAtMs = r.EndedAtMs.Int64
		}
		if r.Cwd.Valid {
			d.Cwd = r.Cwd.String
		}
		if r.FirstPrompt.Valid {
			d.FirstPrompt = r.FirstPrompt.String
		}
		if r.LatestSummary.Valid {
			d.Summary = r.LatestSummary.String
		}
		out = append(out, d)
	}
	return out
}

// cachedLLMInput is the shared input shape for reflect and propose.
// Both features follow identical orchestration; this type lets us
// keep the runCachedLLM helper below private and small.
type cachedLLMInput struct {
	kind   store.LLMOutputKind
	hash   string
	req    llm.Request
	model  string
	force  bool
	output io.Writer
}

// runCachedLLM implements the cache-first / lazy-client / clean-on-
// failure dance. Returns the persisted row id.
func runCachedLLM(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	in cachedLLMInput,
) (int64, error) {
	if !in.force {
		cached, err := store.LoadLLMOutputByHash(s.DB(), in.kind, in.hash)
		if err != nil {
			return 0, fmt.Errorf("%s: cache lookup: %w", in.kind, err)
		}
		if cached != nil {
			_, _ = fmt.Fprint(in.output, cached.Body)
			if !endsWithNewline(cached.Body) {
				_, _ = fmt.Fprintln(in.output)
			}
			return cached.ID, nil
		}
	}

	client, err := newClient()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", in.kind, err)
	}

	req := in.req
	if in.model != "" {
		req.Model = in.model
	}
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("%s: LLM call: %w", in.kind, err)
	}
	if resp.Text == "" {
		return 0, fmt.Errorf("%s: LLM returned empty text", in.kind)
	}

	id, err := persistSummary(s, &persistInput{
		// session_id intentionally empty: reflect/propose span many
		// sessions. Summary uses this same helper with a real id.
		kind:       in.kind,
		hash:       in.hash,
		model:      resp.Model,
		inputToks:  resp.Usage.InputTokens,
		outputToks: resp.Usage.OutputTokens,
		body:       resp.Text,
	})
	if err != nil {
		return 0, err
	}

	_, _ = fmt.Fprint(in.output, resp.Text)
	if !endsWithNewline(resp.Text) {
		_, _ = fmt.Fprintln(in.output)
	}
	return id, nil
}
