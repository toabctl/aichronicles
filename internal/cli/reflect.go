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

// defaultMetaLLMTimeout covers both reflect and propose when
// [limits].reflect_timeout isn't set. Same network pattern as
// summarize, slightly larger prompts, so we give it a longer budget.
const defaultMetaLLMTimeout = 5 * time.Minute

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
		since   time.Duration
		limit   int
		model   string
		force   bool
		jsonOut bool
		dbPath  string
	)
	cmd := &cobra.Command{
		Use:   "reflect",
		Short: "LLM-derived meta-analysis of recent sessions",
		Long: "Looks at sessions that ended within --since and asks the LLM,\n" +
			"via the record_reflection tool, to identify recurring task types,\n" +
			"recurring sources of friction, and one workflow change worth\n" +
			"trying. Existing per-session summaries (from `aichronicles\n" +
			"summarize`) are preferred to raw first prompts.\n\n" +
			"Cached like summarize: same digest list = same prompt_hash = same\n" +
			"cached body. Use --force to re-call. Use --json to emit the raw\n" +
			"JSON body instead of the human-readable render.\n\n" +
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

			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := llmConfigFromFile(cfg.LLM)

			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()

			_, err = RunReflect(ctx, s,
				func() (llm.Client, error) {
					return llm.FromConfig(ctx, llmCfg)
				},
				ReflectOptions{Since: since, Limit: limit, Model: model, Force: force, JSON: jsonOut},
				cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().DurationVar(&since, "since", defaultReflectWindow, "only consider sessions whose ended_at is within this window (e.g. 168h)")
	cmd.Flags().IntVar(&limit, "limit", defaultReflectLimit, "max sessions to feed the LLM, newest first")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON body instead of the human-readable render")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// ReflectOptions drives RunReflect.
type ReflectOptions struct {
	Since time.Duration
	Limit int
	Model string
	Force bool
	JSON  bool
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
	rows, err := store.LoadRecentSessionDigests(ctx, s.DB(), sinceMs, opts.Limit)
	if err != nil {
		return 0, fmt.Errorf("reflect: load sessions: %w", err)
	}
	if len(rows) == 0 {
		return 0, errors.New("reflect: no sessions in the requested window")
	}

	digests, err := digestsFromRowsWithLinks(ctx, s, rows)
	if err != nil {
		return 0, fmt.Errorf("reflect: enrich digests: %w", err)
	}
	built, err := prompts.BuildReflect(digests, window)
	if err != nil {
		return 0, fmt.Errorf("reflect: build prompt: %w", err)
	}

	return runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:     store.LLMKindReflect,
		toolName: prompts.ToolNameReflection,
		result:   new(prompts.ReflectionResult),
		hash:     built.Hash,
		req:      built.Request,
		model:    opts.Model,
		force:    opts.Force,
		jsonRaw:  opts.JSON,
		output:   out,
	})
}

// digestsFromRowsWithLinks converts the DB-facing digest rows into
// the prompt-facing shape and enriches each with the per-session URL
// list from extractions. NULL summary → empty string (BuildReflect
// treats it as "use first_prompt"). Does one extractions query per
// session — N is bounded by reflect/propose --limit (typically ≤25),
// so batching isn't worth the complexity today.
func digestsFromRowsWithLinks(
	ctx context.Context,
	s *store.Store,
	rows []store.SessionDigestRow,
) ([]prompts.SessionDigest, error) {
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
		urls, err := store.LoadExtractionsForSession(ctx, s.DB(), r.ID, "url")
		if err != nil {
			return nil, fmt.Errorf("links for %s: %w", r.ID, err)
		}
		if len(urls) > 0 {
			d.Links = make([]string, len(urls))
			for i, u := range urls {
				d.Links[i] = u.Value
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// cachedLLMInput is the shared input shape for reflect and propose.
// Both features follow identical orchestration: cache-first, tool-
// based LLM call, parse into a typed *Result, persist raw JSON body,
// render (or emit raw).
type cachedLLMInput struct {
	kind     store.LLMOutputKind
	toolName string
	// result is a pointer to the *Result struct the tool payload will
	// decode into. Caller owns the allocation so runCachedLLM does
	// not need to know the concrete type.
	result  any
	hash    string
	req     llm.Request
	model   string
	force   bool
	jsonRaw bool
	output  io.Writer
}

// runCachedLLM implements the cache-first / lazy-client / clean-on-
// failure dance for reflect and propose. Returns the persisted row id.
func runCachedLLM(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	in cachedLLMInput,
) (int64, error) {
	if !in.force {
		cached, err := store.LoadLLMOutputByHash(ctx, s.DB(), in.kind, in.hash)
		if err != nil {
			return 0, fmt.Errorf("%s: cache lookup: %w", in.kind, err)
		}
		if cached != nil {
			if renderErr := emitLLMBody(in.output, in.kind, cached.Body, in.jsonRaw); renderErr != nil {
				return cached.ID, fmt.Errorf("%s: render cached body: %w", in.kind, renderErr)
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
	if err := parseToolResult(resp, in.toolName, in.result); err != nil {
		return 0, fmt.Errorf("%s: %w", in.kind, err)
	}
	body, err := marshalLLMBody(in.result)
	if err != nil {
		return 0, fmt.Errorf("%s: marshal result: %w", in.kind, err)
	}

	id, err := persistSummary(ctx, s, &persistInput{
		// session_id intentionally empty: reflect/propose span many
		// sessions. Summary uses this same helper with a real id.
		kind:       in.kind,
		hash:       in.hash,
		model:      resp.Model,
		inputToks:  resp.Usage.InputTokens,
		outputToks: resp.Usage.OutputTokens,
		body:       body,
	})
	if err != nil {
		return 0, err
	}

	if renderErr := emitLLMBody(in.output, in.kind, body, in.jsonRaw); renderErr != nil {
		return id, fmt.Errorf("%s: render body: %w", in.kind, renderErr)
	}
	return id, nil
}
