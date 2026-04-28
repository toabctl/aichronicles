package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// defaultEmbedKinds names the event kinds whose content_text is worth
// vectorising. Tool results, errors, and most-of-the-rest carry
// embeddings of dubious value (huge, structured, often non-prose);
// the user-prompt + assistant-message + tool-use surface is what a
// "did I work on X" search actually wants to retrieve.
//
// Empty list (--kind="") in the CLI means "all kinds" — the user can
// override when they want full-corpus coverage.
var defaultEmbedKinds = []string{"user_prompt", "assistant_message", "tool_use"}

// defaultEmbedBatch is how many inputs we send per OpenAI embeddings
// call. The API accepts up to 2048 inputs per request and 8192 tokens
// per input, but we keep the batch small so a single failure doesn't
// dump a wall of work — and so the progress-line cadence is useful.
const defaultEmbedBatch = 64

// embedSnippetRunes caps how much of any one event's content_text we
// send to the embedder. text-embedding-3-small handles up to 8192
// input tokens; ~6000 runes of UTF-8 keeps us comfortably under that
// for most languages. Truncating ALSO bounds spend per backfill: a
// stray 100KB tool-output blob would otherwise drag the per-row cost
// up by an order of magnitude.
const embedSnippetRunes = 6000

func newEmbedCmd() *cobra.Command {
	var (
		dbPath  string
		since   time.Duration
		limit   int
		batch   int
		kinds   []string
		modelID string
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "embed",
		Short: "Compute and store vector embeddings for events lacking them",
		Long: "Walks the events table, finds rows with no embedding for the\n" +
			"target model, and posts batched requests to the OpenAI\n" +
			"embeddings endpoint. The resulting float32 vectors land in\n" +
			"the event_embeddings table for `aichronicles search\n" +
			"--semantic` to score against.\n\n" +
			"Idempotent: re-running picks up where a previous run left\n" +
			"off (no row → embed; existing row for the same model →\n" +
			"skip). A model upgrade (text-embedding-3-small → -3-large)\n" +
			"can be done with `--model` set to the new id; old rows for\n" +
			"the previous model are left in place until you re-embed\n" +
			"or prune.\n\n" +
			"Requires OpenAI configured under [llm] (provider=openai).\n" +
			"Anthropic does not expose a hosted embeddings endpoint, so\n" +
			"this command refuses under provider=anthropic rather than\n" +
			"silently downgrading to a different vector space.",
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

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)

			model := modelID
			if model == "" {
				model = llm.DefaultEmbeddingModel
			}
			batchSize := batch
			if batchSize <= 0 {
				batchSize = defaultEmbedBatch
			}
			filter := store.EmbeddingCandidateFilter{
				Model: model,
				Kinds: kinds,
			}
			if since > 0 {
				filter.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			if limit > 0 {
				filter.Limit = limit
			}

			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(),
				&slog.HandlerOptions{Level: slog.LevelInfo})).With("cmd", "aichronicles embed")

			ctx := cmd.Context()
			missing, err := store.CountMissingEmbeddings(ctx, s.DB(), filter)
			if err != nil {
				return fmt.Errorf("count missing: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "model=%s missing=%d batch=%d\n",
				model, missing, batchSize)
			if missing == 0 {
				return nil
			}
			if dryRun {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(dry-run; pass --yes to actually embed)")
				return nil
			}

			embedder, err := llm.EmbedderFromConfig(ctx, llmCfg)
			if err != nil {
				return err
			}

			summary, err := runEmbedLoop(ctx, s, embedder, model, batchSize, filter, log)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), summary)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFlexDurationFlag(cmd, &since, "since", 0,
		"only embed events from the last N (e.g. 7d, 24h)")
	cmd.Flags().IntVar(&limit, "limit", 0,
		"cap the number of events embedded this run (0 = no cap; resume on next run)")
	cmd.Flags().IntVar(&batch, "batch", defaultEmbedBatch,
		"how many inputs per OpenAI embeddings request")
	cmd.Flags().StringSliceVar(&kinds, "kind", defaultEmbedKinds,
		"limit to event kinds (default: user_prompt, assistant_message, tool_use; pass empty to embed all)")
	cmd.Flags().StringVar(&modelID, "model", "",
		"embedding model id (default: "+llm.DefaultEmbeddingModel+")")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the work plan and exit without calling the API")
	return cmd
}

// embedRunSummary is the success-line summary RunEmbed prints. Kept
// as a function so tests can assert on the format.
type embedRunSummary struct {
	EmbeddedRows int
	BatchesRun   int
	InputTokens  int
	Elapsed      time.Duration
}

func (s embedRunSummary) String() string {
	return fmt.Sprintf("embedded %d rows in %d batches (%d input tokens, %s)",
		s.EmbeddedRows, s.BatchesRun, s.InputTokens, s.Elapsed.Round(time.Millisecond))
}

// runEmbedLoop is the inner loop: page through candidates, batch them,
// embed, persist. Returns once the candidate stream is empty.
//
// The LIST → embed → SAVE cycle holds no database transaction across
// the network call — embeddings can take seconds at provider load,
// and an open SQLite write tx would block other writers (including
// the daemon) for that whole window.
func runEmbedLoop(
	ctx context.Context,
	s *store.Store,
	embedder llm.Embedder,
	model string,
	batchSize int,
	filter store.EmbeddingCandidateFilter,
	log *slog.Logger,
) (string, error) {
	if model == "" {
		return "", errors.New("runEmbedLoop: model is required")
	}
	if batchSize <= 0 {
		batchSize = defaultEmbedBatch
	}
	start := time.Now()
	summary := embedRunSummary{}

	for {
		// Page-by-batch: fetch one batch worth of candidates, embed,
		// persist, repeat. Filter.Limit (if set by --limit) caps the
		// total work; we honour it across batches.
		pageFilter := filter
		if filter.Limit > 0 {
			remaining := filter.Limit - summary.EmbeddedRows
			if remaining <= 0 {
				break
			}
			if remaining < batchSize {
				pageFilter.Limit = remaining
			} else {
				pageFilter.Limit = batchSize
			}
		} else {
			pageFilter.Limit = batchSize
		}

		candidates, err := store.ListEventsWithoutEmbedding(ctx, s.DB(), pageFilter)
		if err != nil {
			return "", fmt.Errorf("list candidates: %w", err)
		}
		if len(candidates) == 0 {
			break
		}

		inputs := make([]string, len(candidates))
		for i, c := range candidates {
			inputs[i] = truncateForEmbedding(c.ContentText)
		}

		resp, err := embedder.Embed(ctx, llm.EmbedRequest{
			Model:  model,
			Inputs: inputs,
		})
		if err != nil {
			return "", fmt.Errorf("embed batch: %w", err)
		}
		if len(resp.Vectors) != len(candidates) {
			return "", fmt.Errorf("embed: %d vectors for %d inputs", len(resp.Vectors), len(candidates))
		}

		nowMs := time.Now().UnixMilli()
		for i, c := range candidates {
			if err := store.SaveEmbedding(ctx, s.DB(), store.Embedding{
				EventID:     c.EventID,
				Model:       model,
				Dim:         len(resp.Vectors[i]),
				Vec:         resp.Vectors[i],
				CreatedAtMs: nowMs,
			}); err != nil {
				return "", fmt.Errorf("save embedding for %s: %w", c.EventID, err)
			}
		}
		summary.EmbeddedRows += len(candidates)
		summary.BatchesRun++
		summary.InputTokens += resp.Usage.InputTokens

		log.Info("embed batch", "rows", len(candidates),
			"total", summary.EmbeddedRows, "tokens_so_far", summary.InputTokens)

		// If the page returned fewer than the batch size, the backlog
		// is exhausted — no need to query again. Cheap optimisation
		// that also makes test loops terminate without an empty trailing
		// query.
		if len(candidates) < pageFilter.Limit {
			break
		}
	}

	summary.Elapsed = time.Since(start)
	return summary.String(), nil
}

// truncateForEmbedding caps content_text by rune count so the
// embedder request stays under the per-input token budget. Strips
// control whitespace too — embeddings models don't care about layout
// and trimming saves a small fraction of tokens.
func truncateForEmbedding(s string) string {
	flat := strings.ReplaceAll(s, "\r", " ")
	runes := []rune(flat)
	if len(runes) <= embedSnippetRunes {
		return flat
	}
	return string(runes[:embedSnippetRunes])
}
