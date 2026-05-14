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

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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
		since    time.Duration
		limit    int
		model    string
		force    bool
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "reflect",
		Short: "LLM-derived meta-analysis of recent sessions",
		Long: "Looks at sessions that ended within --since and asks the LLM,\n" +
			"via the record_reflection tool, to identify recurring task types,\n" +
			"recurring sources of friction, and one workflow change worth\n" +
			"trying. Existing per-session summaries (from `aichronicles\n" +
			"summarize`) are preferred to raw first prompts.\n\n" +
			"Cached like summarize: same digest list = same prompt_hash =\n" +
			"same cached body. Use --force to re-call. Use --format=json to\n" +
			"emit the raw JSON body instead of the human-readable render.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}

			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)

			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()

			_, err = RunReflect(ctx, c,
				func() (llm.Client, error) {
					return llm.FromConfig(ctx, llmCfg)
				},
				ReflectOptions{Since: since, Limit: limit, Model: model, Force: force, JSON: format == FormatJSON},
				cmd.OutOrStdout())
			return err
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultReflectWindow, "only consider sessions whose ended_at is within this window (e.g. 24h, 7d)")
	cmd.Flags().IntVar(&limit, "limit", defaultReflectLimit, "max sessions to feed the LLM, newest first")
	addModelFlag(cmd, &model)
	addForceLLMCacheFlag(cmd, &force)
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
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
//
// All reads (session digests, per-session extractions, outcome
// backfill) go through aichronicles-api. The cache lookup + persist
// also routes through the api so the single-writer invariant on
// llm_outputs is preserved.
func RunReflect(
	ctx context.Context,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts ReflectOptions,
	out io.Writer,
) (int64, error) {
	window := opts.Since
	if window <= 0 {
		window = defaultReflectWindow
	}

	sinceMs := time.Now().Add(-window).UnixMilli()
	resp, err := c.SessionDigests(ctx, sinceMs, opts.Limit)
	if err != nil {
		return 0, fmt.Errorf("reflect: load sessions: %w", err)
	}
	if len(resp.Digests) == 0 {
		return 0, errors.New("reflect: no sessions in the requested window")
	}

	digests, err := digestsFromRowsWithLinks(ctx, c, resp.Digests)
	if err != nil {
		return 0, fmt.Errorf("reflect: enrich digests: %w", err)
	}
	built, err := prompts.BuildReflect(digests, window)
	if err != nil {
		return 0, fmt.Errorf("reflect: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("reflect: egress redaction fired",
			"patterns", strings.Join(built.Patterns, ","))
	}

	return runCachedLLM(ctx, c, newClient, cachedLLMInput{
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

// digestsFromRowsWithLinks converts the wire digest rows into the
// prompt-facing shape and enriches each with the per-session URL +
// shell-command extractions and the outcome cue. NULL summary →
// dropped; BuildReflect / BuildPropose require ≥2 summarised rows.
//
// Per-session enrichment is three api round-trips (extractions×2 +
// outcome). N is bounded by --limit (typically ≤25), so the
// fan-out is fine; batching would require new "for these N
// sessions" endpoints and the win is below the noise floor at this
// scale.
func digestsFromRowsWithLinks(
	ctx context.Context,
	c *apiclient.Client,
	rows []wire.SessionDigest,
) ([]prompts.SessionDigest, error) {
	out := make([]prompts.SessionDigest, 0, len(rows))
	skipped := make([]string, 0)
	for _, r := range rows {
		summary := ""
		if r.LatestSummary != nil {
			summary = strings.TrimSpace(*r.LatestSummary)
		}
		if summary == "" {
			skipped = append(skipped, r.ID)
			continue
		}

		d := prompts.SessionDigest{ID: r.ID, Summary: summary}
		if r.StartedAtMs != nil {
			d.StartedAtMs = *r.StartedAtMs
		}
		if r.EndedAtMs != nil {
			d.EndedAtMs = *r.EndedAtMs
		}
		if r.Cwd != nil {
			d.Cwd = *r.Cwd
		}
		if r.FirstPrompt != nil {
			d.FirstPrompt = *r.FirstPrompt
		}
		urls, err := c.SessionExtractions(ctx, r.ID, "url")
		if err != nil {
			return nil, fmt.Errorf("links for %s: %w", r.ID, err)
		}
		if len(urls.Extractions) > 0 {
			d.Links = make([]string, len(urls.Extractions))
			for i, u := range urls.Extractions {
				d.Links[i] = u.Value
			}
		}
		shells, err := c.SessionExtractions(ctx, r.ID, "shell_command")
		if err != nil {
			return nil, fmt.Errorf("shell commands for %s: %w", r.ID, err)
		}
		if len(shells.Extractions) > 0 {
			d.ShellCommands = make([]string, len(shells.Extractions))
			for i, sc := range shells.Extractions {
				d.ShellCommands[i] = sc.Value
			}
		}
		// Outcome enrichment: read-or-backfill via the api. Best-
		// effort — a failure downgrades to "no outcome cue" rather
		// than blocking the whole digest.
		if outcome, oerr := c.SessionOutcome(ctx, r.ID); oerr != nil {
			slog.Warn("digest: skipping outcome cue", "session", r.ID, "err", oerr)
		} else {
			d.Outcome = &outcome
		}
		out = append(out, d)
	}
	if len(skipped) > 0 {
		slog.Info("digests: skipped sessions without summary",
			"count", len(skipped),
			"hint", "run `aichronicles summarize <session-id>` to include them next time")
	}
	if len(out) < 2 {
		return nil, fmt.Errorf(
			"need ≥2 sessions with summaries to reflect/propose; %d of %d in window are summarized. "+
				"Run `aichronicles summaries missing --since <window>` to see candidates, "+
				"then `aichronicles summaries fill --since <window>` to fill them in one shot",
			len(out), len(rows))
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
	// sessionID, when non-empty, attributes the output row to one
	// session. Empty for multi-session features (reflect, propose);
	// set for single-session features (summary, induction) so the
	// CLI listing can join back to sessions cleanly.
	sessionID string
	// renderBody, when non-nil, transforms the persisted body before
	// rendering — anti-fabrication filters (e.g. dropping
	// challenge.grounded_in entries the model can't ground in the
	// canonical input lists) run here. The stored llm_outputs.body
	// is what the LLM emitted; the rendered output is what passes
	// the post-decode anchor check. Callers that don't need the
	// hook leave this nil and rendering uses the raw body.
	renderBody func(body string) (string, error)
}

// runCachedLLM implements the cache-first / lazy-client / clean-on-
// failure dance for reflect, propose, summarize, induction, facts,
// and skills evolve. Returns the persisted row id.
//
// Cache lookup goes through GET /v1/llm-outputs?kind=&prompt_hash=
// (ErrNotFound is the cache-miss signal); persistence goes through
// POST /v1/llm-outputs. The api owns the single writer lock — every
// LLM-deriving CLI converging on this helper keeps the writer
// invariant intact even when those CLIs still do their own
// enrichment reads.
func runCachedLLM(
	ctx context.Context,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	in cachedLLMInput,
) (int64, error) {
	if !in.force {
		cached, err := c.LLMOutputByHash(ctx, string(in.kind), in.hash)
		switch {
		case err == nil:
			// Populate in.result from the cached body so callers
			// can read the parsed result uniformly across hit and
			// miss paths (the miss path populates it via
			// parseToolResult below). Without this, hooks that act
			// on the parsed result — e.g. recording skill_candidates
			// after RunPropose / RunInductionForSession — only fire
			// on cache misses, breaking the lifecycle invariant.
			if in.result != nil {
				if uerr := unmarshalLLMBody(cached.Body, in.result); uerr != nil {
					return cached.ID, fmt.Errorf("%s: parse cached body: %w", in.kind, uerr)
				}
			}
			renderBody := cached.Body
			if in.renderBody != nil {
				rb, rerr := in.renderBody(renderBody)
				if rerr != nil {
					return cached.ID, fmt.Errorf("%s: filter cached body: %w", in.kind, rerr)
				}
				renderBody = rb
			}
			if renderErr := emitLLMBody(in.output, in.kind, renderBody, in.jsonRaw); renderErr != nil {
				return cached.ID, fmt.Errorf("%s: render cached body: %w", in.kind, renderErr)
			}
			return cached.ID, nil
		case errors.Is(err, apiclient.ErrNotFound):
			// fall through to the LLM call
		default:
			return 0, fmt.Errorf("%s: cache lookup: %w", in.kind, err)
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

	saveReq := wire.SaveLLMOutputRequest{
		// session_id intentionally empty for reflect/propose:
		// they span many sessions. Single-session callers
		// (summary, induction, facts) populate it.
		Kind:        string(in.kind),
		Model:       resp.Model,
		PromptHash:  in.hash,
		Body:        body,
		CreatedAtMs: time.Now().UnixMilli(),
	}
	if in.sessionID != "" {
		sid := in.sessionID
		saveReq.SessionID = &sid
	}
	if resp.Usage.InputTokens > 0 {
		v := int64(resp.Usage.InputTokens)
		saveReq.InputTokens = &v
	}
	if resp.Usage.OutputTokens > 0 {
		v := int64(resp.Usage.OutputTokens)
		saveReq.OutputTokens = &v
	}
	saveResp, err := c.SaveLLMOutput(ctx, saveReq)
	if err != nil {
		return 0, fmt.Errorf("%s: persist: %w", in.kind, err)
	}

	renderBody := body
	if in.renderBody != nil {
		rb, rerr := in.renderBody(renderBody)
		if rerr != nil {
			return saveResp.ID, fmt.Errorf("%s: filter body: %w", in.kind, rerr)
		}
		renderBody = rb
	}
	if renderErr := emitLLMBody(in.output, in.kind, renderBody, in.jsonRaw); renderErr != nil {
		return saveResp.ID, fmt.Errorf("%s: render body: %w", in.kind, renderErr)
	}
	return saveResp.ID, nil
}
