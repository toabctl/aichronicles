package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// summarizeTimeout caps the whole subcommand (prompt build + LLM
// round-trip + persist). 3 minutes is well inside Anthropic's own
// deadline for a 1K-token summary but still bounds a wedged network.
const summarizeTimeout = 3 * time.Minute

func newSummarizeCmd() *cobra.Command {
	var (
		sessionID string
		model     string
		force     bool
		jsonOut   bool
		dbPath    string
	)
	cmd := &cobra.Command{
		Use:   "summarize",
		Short: "Generate an LLM summary for one session",
		Long: "Pulls every event for --session, asks the LLM for a structured\n" +
			"summary (topic, what was done, unresolved issues, files touched,\n" +
			"annotated links), and persists the JSON reply in llm_outputs.\n\n" +
			"Idempotent on the full prompt: re-running without --force returns\n" +
			"the cached summary and does not call the LLM again. Pass --force\n" +
			"to bypass the cache (e.g. after changing the prompt template).\n\n" +
			"Output is rendered for the terminal by default; pass --json to\n" +
			"emit the raw JSON body stored in the database.\n\n" +
			"Requires " + llm.APIKeyEnv + " to be set unless --force is off AND\n" +
			"a cached summary exists.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sessionID == "" {
				return errors.New("summarize: --session is required")
			}
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

			// Only build the client lazily — the user should be able
			// to re-print a cached summary without an API key.
			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			newClient := func() (llm.Client, error) {
				return llm.FromEnvOrCommand(cmd.Context(), cfg.LLM.Anthropic.APIKeyCommand)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), summarizeTimeout)
			defer cancel()
			_, err = RunSummarize(ctx, s, newClient, SummarizeOptions{
				SessionID: sessionID, Model: model, Force: force, JSON: jsonOut,
			}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "session id or unique prefix (see `aichronicles sessions`)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON body instead of the human-readable render")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// SummarizeOptions drives RunSummarize. Exported so tests and future
// MCP wiring can hit the same code path without the cobra surface.
type SummarizeOptions struct {
	SessionID string
	Model     string
	Force     bool
	JSON      bool
}

// RunSummarize orchestrates the three phases: build prompt, hit cache
// or call LLM, persist. Writes the final summary to out. Returns the
// stored row's id.
//
// newClient is a constructor — not a pre-built Client — so that a
// cache hit never requires an API key. Only if we actually need the
// LLM do we ask for credentials.
func RunSummarize(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts SummarizeOptions,
	out io.Writer,
) (int64, error) {
	sessionID, err := store.ResolveSessionIDPrefix(ctx, s.DB(), opts.SessionID)
	if err != nil {
		return 0, fmt.Errorf("summarize: %w", err)
	}
	events, err := store.LoadEventsForSession(ctx, s.DB(), sessionID, 0)
	if err != nil {
		return 0, fmt.Errorf("summarize: load events: %w", err)
	}
	if len(events) == 0 {
		return 0, fmt.Errorf("summarize: session %s has no events", sessionID)
	}

	// Pre-extracted URLs for this session — the LLM annotates each
	// with a `used_for` rather than extracting them itself. Empty
	// slice is fine (tool input will carry links:[]).
	urls, err := store.LoadExtractionsForSession(ctx, s.DB(), sessionID, "url")
	if err != nil {
		return 0, fmt.Errorf("summarize: load extractions: %w", err)
	}
	links := make([]string, len(urls))
	for i, u := range urls {
		links[i] = u.Value
	}

	built, err := prompts.BuildSummary(sessionID, events, links)
	if err != nil {
		return 0, fmt.Errorf("summarize: build prompt: %w", err)
	}

	if !opts.Force {
		cached, err := store.LoadLLMOutputByHash(ctx, s.DB(), store.LLMKindSummary, built.Hash)
		if err != nil {
			return 0, fmt.Errorf("summarize: cache lookup: %w", err)
		}
		if cached != nil {
			if renderErr := emitLLMBody(out, store.LLMKindSummary, cached.Body, opts.JSON); renderErr != nil {
				return cached.ID, fmt.Errorf("summarize: render cached body: %w", renderErr)
			}
			return cached.ID, nil
		}
	}

	client, err := newClient()
	if err != nil {
		return 0, fmt.Errorf("summarize: %w", err)
	}

	req := built.Request
	if opts.Model != "" {
		req.Model = opts.Model
	}
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("summarize: LLM call: %w", err)
	}
	var result prompts.SummaryResult
	if err := parseToolResult(resp, prompts.ToolNameSummary, &result); err != nil {
		return 0, fmt.Errorf("summarize: %w", err)
	}
	body, err := marshalLLMBody(&result)
	if err != nil {
		return 0, fmt.Errorf("summarize: marshal result: %w", err)
	}

	id, err := persistSummary(ctx, s, &persistInput{
		sessionID:  sessionID,
		kind:       store.LLMKindSummary,
		hash:       built.Hash,
		model:      resp.Model,
		inputToks:  resp.Usage.InputTokens,
		outputToks: resp.Usage.OutputTokens,
		body:       body,
	})
	if err != nil {
		return 0, err
	}

	if renderErr := emitLLMBody(out, store.LLMKindSummary, body, opts.JSON); renderErr != nil {
		return id, fmt.Errorf("summarize: render body: %w", renderErr)
	}
	return id, nil
}

// persistInput groups every column we fill when storing an LLM
// output. Private to this file; exists so persistSummary's signature
// doesn't sprawl.
type persistInput struct {
	sessionID  string
	kind       store.LLMOutputKind
	hash       string
	model      string
	inputToks  int
	outputToks int
	body       string
}

func persistSummary(ctx context.Context, s *store.Store, in *persistInput) (int64, error) {
	out := &store.LLMOutput{
		SessionID:   sql.NullString{String: in.sessionID, Valid: in.sessionID != ""},
		Kind:        in.kind,
		Model:       in.model,
		PromptHash:  in.hash,
		Body:        in.body,
		CreatedAtMs: time.Now().UnixMilli(),
	}
	if in.inputToks > 0 {
		out.InputTokens = sql.NullInt64{Int64: int64(in.inputToks), Valid: true}
	}
	if in.outputToks > 0 {
		out.OutputTokens = sql.NullInt64{Int64: int64(in.outputToks), Valid: true}
	}

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, _, err := store.SaveLLMOutput(ctx, tx, out)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// marshalLLMBody is the canonical serializer for a tool result we're
// about to stash in llm_outputs.body. Deterministic JSON (no HTML
// escaping, indented for human grep) so identical inputs produce
// identical bytes and line-diff tools work on the stored rows.
func marshalLLMBody(v any) (string, error) {
	b, err := jsonMarshalIndent(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
