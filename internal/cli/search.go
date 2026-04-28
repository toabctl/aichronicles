package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/searchquery"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// maxSnippetRunes caps how much of content_text we print per hit. FTS
// can return multi-KB assistant turns; the CLI is for spotting
// matches, not reading them.
const maxSnippetRunes = 140

func newSearchCmd() *cobra.Command {
	var (
		limit     int
		kind      string
		sessionID string
		since     time.Duration
		dbPath    string
		noDedup   bool
		formatIn  string
		summarize bool
		topN      int
		model     string
		agent     string
		toolName  string
		skillName string
		fileMatch string
		withFails bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search over captured envelopes",
		Long: "Searches across captured envelopes and prints the top hits\n" +
			"one per line. Type plain words; bare tokens match by prefix\n" +
			"(`mongo` finds `mongodb`). Wrap exact matches in double\n" +
			"quotes (`\"panic stack\"`). Identifiers and paths can be\n" +
			"typed verbatim (`migrate.go`). Pass --format=json for a\n" +
			"structured payload suitable for jq.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			// If the user passed a session-id prefix (e.g. the 8-char
			// preview `aichronicles sessions` prints), resolve it to
			// the full id here so the downstream filter is an exact
			// match.
			resolvedSessionID := sessionID
			if resolvedSessionID != "" {
				full, err := store.ResolveSessionIDPrefix(cmd.Context(), s.DB(), resolvedSessionID)
				if err != nil {
					return err
				}
				resolvedSessionID = full
			}

			opts := SearchOptions{
				Query:             args[0],
				Kind:              kind,
				SessionID:         resolvedSessionID,
				Limit:             limit,
				NoDedup:           noDedup,
				Format:            format,
				Summarize:         summarize,
				TopN:              topN,
				Model:             model,
				SourceAgent:       agent,
				ToolName:          toolName,
				SkillName:         skillName,
				FilePathSubstring: fileMatch,
				WithFailures:      withFails,
			}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			if summarize {
				cfg, cfgErr := config.Load()
				if cfgErr != nil {
					return cfgErr
				}
				llmCfg := LLMConfigFromFile(cfg.LLM)
				ctx, cancel := context.WithTimeout(cmd.Context(),
					cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
				defer cancel()
				return RunSearchSummary(ctx, s, opts,
					func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
					cmd.OutOrStdout())
			}
			return RunSearch(s, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "max number of hits")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by event kind (user_prompt, tool_use, …)")
	cmd.Flags().StringVar(&sessionID, "session", "", "filter by session id or unique prefix")
	registerSessionFlagCompletion(cmd)
	addFlexDurationFlag(cmd, &since, "since", 0, "only events within this duration (e.g. 24h, 7d)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().BoolVar(&noDedup, "no-dedup", false, "show every row even when the same turn was captured from multiple sources (hook + import)")
	cmd.Flags().BoolVar(&summarize, "summarize", false, "synthesise an LLM-written answer from the top hits instead of printing rows (requires "+llm.APIKeyEnv+")")
	cmd.Flags().IntVar(&topN, "top", 5, "with --summarize: max hits fed to the LLM as grounding context")
	cmd.Flags().StringVar(&model, "model", "", "with --summarize: LLM model id (default: provider's default)")
	cmd.Flags().StringVar(&agent, "agent", "", "filter by source agent (claude-code | gemini-cli)")
	cmd.Flags().StringVar(&toolName, "tool", "", "filter by tool name (e.g. Bash, run_shell_command)")
	cmd.Flags().StringVar(&skillName, "skill", "", "filter to sessions that loaded this skill")
	cmd.Flags().StringVar(&fileMatch, "file", "", "filter to sessions that touched a file matching this substring")
	cmd.Flags().BoolVar(&withFails, "with-failures", false, "filter to sessions that produced at least one tool_failure event")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// SearchOptions are the flag values passed to RunSearch. Exported so
// tests and future MCP wiring can drive the same search path.
type SearchOptions struct {
	Query     string
	Kind      string
	SessionID string
	SinceMs   int64
	Limit     int
	// NoDedup disables query-time deduplication. By default, when the
	// same logical turn is present from multiple sources (e.g. hook
	// events and transcript imports of the same session), search
	// collapses them to one row per (session_id, role, kind, content),
	// preferring transport=hook. Set to true to surface every row.
	NoDedup bool
	Format  OutputFormat // empty == FormatTable
	// Summarize, when true, asks the LLM to compose a 2-5 sentence
	// answer from the top hits instead of printing rows. Drives
	// RunSearchSummary (which is a separate entrypoint, not toggled
	// inside RunSearch, because it needs an llm.Client).
	Summarize bool
	TopN      int    // with Summarize: max hits fed to the LLM (default 5)
	Model     string // with Summarize: model id override

	// Faceted-search filters. All optional; multiple combine with AND.
	SourceAgent       string // e.g. "claude-code", "gemini-cli"
	ToolName          string // e.g. "Bash", "run_shell_command"
	SkillName         string // sessions that loaded this skill
	FilePathSubstring string // sessions that touched a file matching this substring
	WithFailures      bool   // sessions that produced ≥1 tool_failure event
}

// SearchHitJSON is the JSON shape emitted by `search --format=json`.
// Field names mirror the on-disk events schema. Snippet may be the
// full content_text or a truncation; the truncated flag tells the
// consumer which.
type SearchHitJSON struct {
	SessionID  string `json:"session_id"`
	Kind       string `json:"kind"`
	Cwd        string `json:"cwd,omitempty"`
	TsSourceMs int64  `json:"ts_source_ms"`
	Snippet    string `json:"snippet"`
	Truncated  bool   `json:"truncated"`
}

// RunSearch executes an FTS5 query against the store and writes hits
// to out. Empty query is an error because FTS5 would either error
// itself or return the whole corpus.
//
// The user-facing query (plain words, optionally "quoted phrases")
// is translated into a syntactically-safe FTS5 MATCH expression by
// internal/searchquery before it ever reaches SQLite. Callers should
// not pre-escape — that's the parser's job.
//
// SQL composition lives in internal/store/search.go; this function
// is just the CLI's translation between SearchOptions (cobra flags)
// and store.SearchEventOpts plus the formatting layer.
//
// Format=table renders aligned columns (header + tab-separated rows
// fed through tabwriter), with a "(no hits)" line on an empty result.
// Format=json emits a JSON array of SearchHitJSON values for jq.
func RunSearch(s *store.Store, opts SearchOptions, out io.Writer) error {
	if strings.TrimSpace(opts.Query) == "" {
		return errors.New("search query must not be empty")
	}

	fts, err := searchquery.ToFTS5(opts.Query)
	if err != nil {
		return fmt.Errorf("parse query: %w", err)
	}

	hits, err := store.SearchEvents(context.Background(), s.DB(), store.SearchEventOpts{
		Query:             fts,
		Kind:              opts.Kind,
		SessionID:         opts.SessionID,
		SinceMs:           opts.SinceMs,
		Limit:             opts.Limit,
		NoDedup:           opts.NoDedup,
		SourceAgent:       opts.SourceAgent,
		ToolName:          opts.ToolName,
		SkillName:         opts.SkillName,
		FilePathSubstring: opts.FilePathSubstring,
		WithFailures:      opts.WithFailures,
		// CLI defaults to FTS rank ordering (most relevant first);
		// recency-boosted scoring lands in a follow-up commit.
		Order: store.OrderRank,
	})
	if err != nil {
		return err
	}

	if opts.Format == FormatJSON {
		payload := make([]SearchHitJSON, 0, len(hits))
		for _, h := range hits {
			snippet, truncated := pickSnippet(h)
			payload = append(payload, SearchHitJSON{
				SessionID:  h.SessionID,
				Kind:       h.Kind,
				Cwd:        nullStringValue(h.Cwd),
				TsSourceMs: h.TsSourceMs,
				Snippet:    snippet,
				Truncated:  truncated,
			})
		}
		return emitJSON(out, payload)
	}

	if len(hits) == 0 {
		_, err := fmt.Fprintf(out, "(no hits for %q)\n", opts.Query)
		return err
	}

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "WHEN\tSESSION\tKIND\tCWD\tCONTENT"); err != nil {
		return err
	}
	for _, h := range hits {
		snippet, _ := pickSnippet(h)
		_, _ = fmt.Fprintln(tw, formatHit(h.SessionID, h.Kind,
			nullStringValue(h.Cwd), h.TsSourceMs, snippet))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = io.Copy(out, &buf)
	return err
}

// nullStringValue returns the string content of a sql.NullString, or
// empty if NULL. Mirrors the old `deref` helper that operated on
// *string.
func nullStringValue(n sql.NullString) string {
	if !n.Valid {
		return ""
	}
	return n.String
}

// pickSnippet chooses the best available text for a search hit.
// SQLite's snippet() (when present and non-empty) is BM25-aware and
// centers on the matched terms; the Go-side first-N-runes preview
// is the fallback for the rare case where snippet() returned empty.
//
// Returns (display, truncated). truncated is true iff the displayed
// text is shorter than the full content_text — i.e., the user is
// looking at a preview, not the whole row.
func pickSnippet(h store.SearchEventHit) (string, bool) {
	full := nullStringValue(h.Content)
	if h.Snippet.Valid && h.Snippet.String != "" {
		return h.Snippet.String, h.Snippet.String != full
	}
	return snippetWithTruncation(full)
}

// snippetWithTruncation returns (snippet, truncated). Same rune cap
// and newline-flattening as truncateSnippet, but reports whether the
// cap actually fired so JSON consumers can distinguish "full content"
// from "first 140 runes." Keeps --format=table and --format=json
// telling the same story about how much of the content survived.
func snippetWithTruncation(s string) (string, bool) {
	flat := strings.ReplaceAll(s, "\n", " ")
	flat = strings.ReplaceAll(flat, "\r", " ")
	flat = strings.ReplaceAll(flat, "\t", " ")
	runes := []rune(flat)
	if len(runes) <= maxSnippetRunes {
		return flat, false
	}
	return string(runes[:maxSnippetRunes]), true
}

// formatHit renders one row as a tab-separated line. Column alignment
// is handled by the tabwriter in RunSearch; this function only
// prepares the cells.
func formatHit(sessionID, kind, cwd string, tsSourceMs int64, content string) string {
	ts := formatTimeForUser(tsSourceMs, time.Now())
	sess := sessionID
	if len(sess) > 8 {
		sess = sess[:8]
	}
	if cwd == "" {
		cwd = "-"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s", ts, sess, kind, cwd, truncateSnippet(content))
}

// truncateSnippet flattens newlines and caps rune length so hits fit
// on a terminal line. Runes (not bytes) so multibyte UTF-8 doesn't
// split mid-character.
func truncateSnippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	runes := []rune(s)
	if len(runes) <= maxSnippetRunes {
		return s
	}
	return string(runes[:maxSnippetRunes]) + "…"
}

// summarySnippetRunes is a higher cap for snippets fed to the
// summary LLM. The CLI table cap (140) is for visual scanning;
// the LLM benefits from more grounding text per hit (its window
// is the prompt-token budget, not a terminal line). Still bounded
// so a giant assistant turn doesn't blow up the request.
const summarySnippetRunes = 1200

// defaultSummaryMaxTokens caps the LLM response at a tight budget —
// search summaries are short by spec (2–5 sentences). Override via
// future flag if a real use case wants longer.
const defaultSummaryMaxTokens = 512

// RunSearchSummary runs a normal FTS search and then, instead of
// printing rows, asks the LLM to synthesise a grounded answer
// citing the top hits' session_ids. JSON format wraps the answer
// alongside the hits used so jq consumers can show both.
func RunSearchSummary(
	ctx context.Context,
	s *store.Store,
	opts SearchOptions,
	newClient func() (llm.Client, error),
	out io.Writer,
) error {
	if strings.TrimSpace(opts.Query) == "" {
		return errors.New("search query must not be empty")
	}
	fts, err := searchquery.ToFTS5(opts.Query)
	if err != nil {
		return fmt.Errorf("parse query: %w", err)
	}

	// Cap top-N: even if the user passed --limit=200, summary should
	// only feed the top few hits to the LLM. Default 5; clamp at
	// what was returned.
	topN := opts.TopN
	if topN <= 0 {
		topN = 5
	}
	hits, err := store.SearchEvents(ctx, s.DB(), store.SearchEventOpts{
		Query:             fts,
		Kind:              opts.Kind,
		SessionID:         opts.SessionID,
		SinceMs:           opts.SinceMs,
		Limit:             topN,
		NoDedup:           opts.NoDedup,
		SourceAgent:       opts.SourceAgent,
		ToolName:          opts.ToolName,
		SkillName:         opts.SkillName,
		FilePathSubstring: opts.FilePathSubstring,
		WithFailures:      opts.WithFailures,
		Order:             store.OrderRank,
	})
	if err != nil {
		return err
	}

	if len(hits) == 0 {
		if opts.Format == FormatJSON {
			return emitJSON(out, map[string]any{"query": opts.Query, "hits": []any{}, "summary": ""})
		}
		_, err := fmt.Fprintf(out, "(no hits for %q)\n", opts.Query)
		return err
	}

	promptHits := make([]prompts.SearchHit, 0, len(hits))
	for _, h := range hits {
		full := nullStringValue(h.Content)
		flat := strings.ReplaceAll(full, "\r", " ")
		runes := []rune(flat)
		if len(runes) > summarySnippetRunes {
			flat = string(runes[:summarySnippetRunes]) + "…"
		}
		promptHits = append(promptHits, prompts.SearchHit{
			SessionID:  h.SessionID,
			Kind:       h.Kind,
			Cwd:        nullStringValue(h.Cwd),
			TsSourceMs: h.TsSourceMs,
			Snippet:    flat,
		})
	}

	built, err := prompts.BuildSearchSummary(opts.Query, promptHits, defaultSummaryMaxTokens)
	if err != nil {
		return fmt.Errorf("build summary prompt: %w", err)
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	req := built.Request
	if opts.Model != "" {
		req.Model = opts.Model
	}
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return fmt.Errorf("LLM call: %w", err)
	}

	if opts.Format == FormatJSON {
		// JSON consumers get both the synthesised answer and the hits
		// it was grounded in, so they can render link-throughs to the
		// underlying sessions without a second round-trip.
		hitPayload := make([]SearchHitJSON, 0, len(hits))
		for _, h := range hits {
			snippet, truncated := pickSnippet(h)
			hitPayload = append(hitPayload, SearchHitJSON{
				SessionID:  h.SessionID,
				Kind:       h.Kind,
				Cwd:        nullStringValue(h.Cwd),
				TsSourceMs: h.TsSourceMs,
				Snippet:    snippet,
				Truncated:  truncated,
			})
		}
		return emitJSON(out, map[string]any{
			"query":   opts.Query,
			"summary": resp.Text,
			"hits":    hitPayload,
		})
	}

	_, err = fmt.Fprintln(out, resp.Text)
	return err
}
