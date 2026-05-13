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

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultSummarizeTimeout caps the whole subcommand (prompt build +
// LLM round-trip + persist) when [limits].summarize_timeout isn't
// set. 3 minutes is well inside Anthropic's own deadline for a
// 1K-token summary but still bounds a wedged network.
const defaultSummarizeTimeout = 3 * time.Minute

func newSummarizeCmd() *cobra.Command {
	var (
		model    string
		force    bool
		formatIn string
	)
	var sockFlag string
	cmd := &cobra.Command{
		Use:   "summarize <session>",
		Short: "Generate an LLM summary for one session",
		Long: "Pulls every event for the given session, asks the LLM for a\n" +
			"structured summary (topic, what was done, unresolved issues,\n" +
			"files touched, annotated links), and persists the JSON reply\n" +
			"in llm_outputs. Session id may be a unique prefix (see\n" +
			"`aichronicles sessions`).\n\n" +
			"Idempotent on the full prompt: re-running without --force returns\n" +
			"the cached summary and does not call the LLM again. Pass --force\n" +
			"to bypass the cache (e.g. after changing the prompt template).\n\n" +
			"Output is rendered for the terminal by default; pass\n" +
			"--format=json to emit the raw JSON body stored in the\n" +
			"database.\n\n" +
			"Requires " + llm.APIKeyEnv + " to be set unless --force is off AND\n" +
			"a cached summary exists.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSessionID,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}

			// Only build the client lazily — the user should be able
			// to re-print a cached summary without an API key.
			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)
			newClient := func() (llm.Client, error) {
				return llm.FromConfig(cmd.Context(), llmCfg)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout))
			defer cancel()
			_, err = RunSummarize(ctx, c, newClient, SummarizeOptions{
				SessionID: sessionID, Model: model, Force: force, JSON: format == FormatJSON,
			}, cmd.OutOrStdout())
			return err
		},
	}
	addModelFlag(cmd, &model)
	addForceLLMCacheFlag(cmd, &force)
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
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
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts SummarizeOptions,
	out io.Writer,
) (int64, error) {
	sessionID, err := c.ResolveSession(ctx, opts.SessionID)
	if err != nil {
		return 0, fmt.Errorf("summarize: %w", err)
	}
	evResp, err := c.SessionEvents(ctx, sessionID, 0, false)
	if err != nil {
		return 0, fmt.Errorf("summarize: load events: %w", err)
	}
	if len(evResp.Events) == 0 {
		return 0, fmt.Errorf("summarize: session %s has no events", sessionID)
	}
	storeEvents := wireEventsToStore(evResp.Events)

	// Pre-extracted URLs for this session — the LLM annotates each
	// with a `used_for` rather than extracting them itself. Empty
	// slice is fine (tool input will carry links:[]).
	urls, err := c.SessionExtractions(ctx, sessionID, "url")
	if err != nil {
		return 0, fmt.Errorf("summarize: load extractions (url): %w", err)
	}
	links := make([]string, len(urls.Extractions))
	for i, u := range urls.Extractions {
		links[i] = u.Value
	}

	// Pre-extracted file paths — same anti-fabrication grounding the
	// links list provides for URLs. The model is told to draw
	// key_files from this list when present and to copy prose-mention
	// paths verbatim otherwise; absolute paths thanks to
	// FilePathExtractor's cwd-anchoring.
	fileExt, err := c.SessionExtractions(ctx, sessionID, "file_path")
	if err != nil {
		return 0, fmt.Errorf("summarize: load extractions (file_path): %w", err)
	}
	// Multiple Read/Edit calls on the same path produce multiple
	// extraction rows; the prompt only wants distinct values.
	seenFile := make(map[string]struct{}, len(fileExt.Extractions))
	files := make([]string, 0, len(fileExt.Extractions))
	for _, fx := range fileExt.Extractions {
		if _, dup := seenFile[fx.Value]; dup {
			continue
		}
		seenFile[fx.Value] = struct{}{}
		files = append(files, fx.Value)
	}

	// Recent same-cwd sessions the model is allowed to emit
	// session_links to. Bounded shortlist so the prompt stays
	// compact; same anchor — "ended before this session started" —
	// that prevents the LLM from fabricating a connection across
	// overlapping timelines. Recency is the only ranking signal
	// after Round 12 dropped the embedding system; that was the
	// pre-Round-3 default and is the correct behaviour for this
	// retrieval surface.
	candResp, err := c.SessionCandidatePriors(ctx, sessionID, 10)
	if err != nil {
		return 0, fmt.Errorf("summarize: load prior sessions: %w", err)
	}

	priorForPrompt := make([]prompts.CandidatePriorSession, 0, len(candResp.Candidates))
	candidateIDs := make(map[string]struct{}, len(candResp.Candidates))
	for _, cand := range candResp.Candidates {
		priorForPrompt = append(priorForPrompt, prompts.CandidatePriorSession{
			ID:          cand.ID,
			StartedAtMs: cand.StartedAtMs,
			EndedAtMs:   cand.EndedAtMs,
			Topic:       cand.Topic,
		})
		candidateIDs[cand.ID] = struct{}{}
	}

	built, err := prompts.BuildSummary(sessionID, storeEvents, prompts.SummaryInputs{
		Links:             links,
		Files:             files,
		CandidatePriorSes: priorForPrompt,
	})
	if err != nil {
		return 0, fmt.Errorf("summarize: build prompt: %w", err)
	}

	// If the egress redactor caught anything during prompt
	// assembly, surface it on stderr. Without this the user has
	// no signal that scrubbing fired at all — `built.Patterns`
	// was being silently dropped before the B5 audit fix.
	if len(built.Patterns) > 0 {
		slog.Info("summarize: egress redaction fired",
			"session_id", sessionID,
			"patterns", strings.Join(built.Patterns, ","))
	}

	if !opts.Force {
		cached, err := c.LLMOutputByHash(ctx, string(store.LLMKindSummary), built.Hash)
		switch {
		case err == nil:
			// fall through with cached populated below
		case errors.Is(err, apiclient.ErrNotFound):
			cached.ID = 0 // sentinel for "no cache" — render-time check uses cached.ID > 0
		default:
			return 0, fmt.Errorf("summarize: cache lookup: %w", err)
		}
		if cached.ID > 0 {
			// On cache hit we still re-derive session_links from the
			// stored body. The links table is a projection, not the
			// source of truth — a user who ran summarize before the
			// links migration shipped should see them populate on
			// the next no-op re-run.
			if err := saveSessionLinksFromBody(ctx, c, sessionID, cached.Body, candidateIDs); err != nil {
				return cached.ID, fmt.Errorf("summarize: persist session_links from cache: %w", err)
			}
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

	saveReq := wire.SaveLLMOutputRequest{
		SessionID:   &sessionID,
		Kind:        string(store.LLMKindSummary),
		Model:       resp.Model,
		PromptHash:  built.Hash,
		Body:        body,
		CreatedAtMs: time.Now().UnixMilli(),
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
		return 0, fmt.Errorf("summarize: persist: %w", err)
	}
	id := saveResp.ID

	// Persist session_links derived from result.SessionLinks. Done
	// after persistSummary so the FK to sessions(id) is irrelevant
	// (we link to candidates, not to this summary row), but we keep
	// it inside the same RunSummarize call so a partial failure
	// surfaces here rather than as an orphaned summary.
	if err := persistSessionLinks(ctx, c, sessionID, result.SessionLinks, candidateIDs); err != nil {
		return id, fmt.Errorf("summarize: persist session_links: %w", err)
	}

	if renderErr := emitLLMBody(out, store.LLMKindSummary, body, opts.JSON); renderErr != nil {
		return id, fmt.Errorf("summarize: render body: %w", renderErr)
	}
	return id, nil
}

// persistSessionLinks filters the model-emitted links down to ones
// whose to_session_id is in `allowed` (the candidate shortlist the
// model was given), then writes them via the api.
//
// Filtering enforces the prompt's anti-fabrication contract: the
// model is told to only emit ids from the candidate stanza, but
// the schema can't constrain that. A rogue id gets silently
// dropped here rather than failing the whole summarize call —
// the structured summary itself is more valuable than the link
// rows, which are advisory.
//
// `allowed` may be empty (no candidates were loaded), in which
// case any emitted link gets dropped. That's the correct
// degenerate behaviour: no candidates → no permitted targets.
func persistSessionLinks(
	ctx context.Context,
	c *apiclient.Client,
	from string,
	emitted []prompts.SessionLinkAnnotation,
	allowed map[string]struct{},
) error {
	links := make([]wire.SessionLink, 0, len(emitted))
	dropped := 0
	for _, l := range emitted {
		if _, ok := allowed[l.ToSessionID]; !ok {
			dropped++
			continue
		}
		if !store.IsValidSessionLinkKind(l.Kind) {
			dropped++
			continue
		}
		links = append(links, wire.SessionLink{
			ToSessionID: l.ToSessionID,
			Kind:        l.Kind,
			Rationale:   l.Rationale,
		})
	}
	if dropped > 0 {
		slog.Info("summarize: dropped fabricated session_links",
			"from_session_id", from, "dropped", dropped, "kept", len(links))
	}
	// Always call SaveSessionLinks (even with empty links) so a
	// re-summarize that emits nothing clears stale rows from a
	// previous run.
	return c.SaveSessionLinks(ctx, wire.SaveSessionLinksRequest{
		FromSessionID: from,
		Links:         links,
	})
}

// saveSessionLinksFromBody re-projects session_links from a stored
// summary JSON body. Used on cache hits — re-running summarize on
// a cached row should rebuild the projection without re-calling
// the LLM. Tolerates malformed bodies (logs and returns nil).
func saveSessionLinksFromBody(
	ctx context.Context,
	c *apiclient.Client,
	from, body string,
	allowed map[string]struct{},
) error {
	var result prompts.SummaryResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		slog.Warn("summarize: cached body unparseable, skipping session_links projection",
			"from_session_id", from, "err", err)
		return nil
	}
	return persistSessionLinks(ctx, c, from, result.SessionLinks, allowed)
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

// unmarshalLLMBody is the inverse of marshalLLMBody. Used by
// runCachedLLM's cache-hit branch to populate in.result from a
// stored body so callers can read the parsed result uniformly
// across hit and miss paths.
func unmarshalLLMBody(body string, target any) error {
	if err := json.Unmarshal([]byte(body), target); err != nil {
		return fmt.Errorf("unmarshal llm_outputs.body: %w", err)
	}
	return nil
}

// wireEventsToStore converts wire.SessionEvent wire rows back into
// the events.EventView shape internal/llm/prompts consumes. Mechanical
// projection: nullable string fields rehydrate the events.NullString
// struct from the wire's *string. Used by RunSummarize after
// pulling /v1/sessions/{id}/events through the apiclient.
func wireEventsToStore(in []wire.SessionEvent) []events.EventView {
	out := make([]events.EventView, 0, len(in))
	for _, e := range in {
		v := events.EventView{
			EventID:    e.EventID,
			Kind:       e.Kind,
			TsSourceMs: e.TsSourceMs,
		}
		if e.Role != nil {
			v.Role = events.NullString{String: *e.Role, Valid: true}
		}
		if e.ContentText != nil {
			v.ContentText = events.NullString{String: *e.ContentText, Valid: true}
		}
		if e.ToolName != nil {
			v.ToolName = events.NullString{String: *e.ToolName, Valid: true}
		}
		if e.SubagentID != nil {
			v.SubagentID = events.NullString{String: *e.SubagentID, Valid: true}
		}
		if e.SubagentType != nil {
			v.SubagentType = events.NullString{String: *e.SubagentType, Valid: true}
		}
		if e.Cwd != nil {
			v.Cwd = events.NullString{String: *e.Cwd, Valid: true}
		}
		out = append(out, v)
	}
	return out
}
