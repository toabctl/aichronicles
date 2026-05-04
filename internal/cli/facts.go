package cli

import (
	"context"
	"database/sql"
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
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

func newFactsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "facts",
		Short: "Typed semantic-fact memory induced from sessions (MIRIX semantic layer)",
		Long: "Facts are typed (subject, predicate, object) triples derived\n" +
			"from a session — project-level claims like 'uses Go 1.26',\n" +
			"'runs tests via go test ./...'. Distinct from skills /\n" +
			"workflows / propose (procedural memory): facts answer\n" +
			"'what is true?' rather than 'how do I do X?'. The retrieval\n" +
			"surface is keyed by subject (typically a cwd) so the next\n" +
			"session that opens in the same project can ground itself\n" +
			"without re-discovering the build/test/deploy contract from\n" +
			"raw events.\n\n" +
			"Subcommands:\n" +
			"  induce   — induce typed facts from one specific session id\n" +
			"  list     — show recent fact inductions (LLM rows)\n" +
			"  show     — show every fact for a given subject (e.g. cwd)\n",
	}
	cmd.AddCommand(newFactsInduceCmd())
	cmd.AddCommand(newFactsListCmd())
	cmd.AddCommand(newFactsShowCmd())
	return cmd
}

func newFactsInduceCmd() *cobra.Command {
	var (
		session  string
		model    string
		force    bool
		dbPath   string
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "induce --session <id>",
		Short: "Induce typed facts from one specific session",
		Long: "Asks the LLM to extract typed (subject, predicate, object)\n" +
			"triples from the named session — project-level facts the\n" +
			"future agent benefits from knowing without re-discovery.\n\n" +
			"Persists the LLM reply in llm_outputs(kind=facts) AND each\n" +
			"individual fact in semantic_facts (the typed retrieval\n" +
			"surface). Re-running on the same session hits the cache;\n" +
			"--force re-calls the LLM. Re-asserting the same fact\n" +
			"upserts in place; conflicting fact objects coexist as\n" +
			"separate rows.\n\n" +
			"The session must have been summarized first.\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if session == "" {
				return errors.New("--session <id> is required")
			}
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
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
				cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout))
			defer cancel()

			_, err = RunFactsForSession(ctx, s, c,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				FactsRunOptions{
					SessionID: session,
					Model:     model,
					Force:     force,
					JSON:      format == FormatJSON,
				}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id (full or unique prefix) to induce facts from")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the cache and re-call the LLM")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newFactsListCmd() *cobra.Command {
	var (
		sockFlag string
		limit    int
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show recent fact inductions (one row per LLM run)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			rows, err := c.LLMOutputsList(cmd.Context(), string(store.LLMKindFacts), "", limit)
			if err != nil {
				return fmt.Errorf("load facts rows: %w", err)
			}
			return renderFactsList(cmd.OutOrStdout(), rows, format)
		},
	}
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to render, newest first")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newFactsShowCmd() *cobra.Command {
	var (
		subject  string
		sockFlag string
		limit    int
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "show --subject <cwd|name>",
		Short: "Show every typed fact known about one subject",
		Long: "Loads every semantic_facts row for the given subject. Use\n" +
			"the cwd path of a project to get its build/test/deploy\n" +
			"contract — this is the v1 retrieval surface for typed\n" +
			"semantic memory.\n",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if subject == "" {
				return errors.New("--subject is required")
			}
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			resp, err := c.Facts(cmd.Context(), subject, limit)
			if err != nil {
				return fmt.Errorf("load facts: %w", err)
			}
			return renderFactsForSubject(cmd.OutOrStdout(), subject, resp.Facts, format)
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "subject (cwd path or other anchor) to load facts for")
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	cmd.Flags().IntVar(&limit, "limit", 100, "max facts to return")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// FactsRunOptions drives RunFactsForSession.
type FactsRunOptions struct {
	SessionID string
	Model     string
	Force     bool
	JSON      bool
}

// RunFactsForSession orchestrates single-session fact induction:
// resolve session prefix → require summary → enrich with extractions
// + outcome → BuildFacts → cached LLM → persist each emitted fact
// into semantic_facts → render. Returns the persisted llm_outputs
// row id.
//
// The two-layer persistence (llm_outputs row for caching +
// semantic_facts rows for typed retrieval) is deliberate: the LLM
// row is the cache key + audit log; semantic_facts is the truth
// surface callers query at retrieval time. Both writes happen in
// the same call so a partial failure is observable on the next
// re-run rather than producing a half-state.
func RunFactsForSession(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts FactsRunOptions,
	out io.Writer,
) (int64, error) {
	sessionID, err := store.ResolveSessionIDPrefix(ctx, s.DB(), opts.SessionID)
	if err != nil {
		return 0, fmt.Errorf("facts: %w", err)
	}

	digestRow, err := store.LoadSessionDigest(ctx, s.DB(), sessionID)
	if err != nil {
		return 0, fmt.Errorf("facts: load digest: %w", err)
	}
	if digestRow == nil {
		return 0, fmt.Errorf("facts: session %s not found", sessionID)
	}
	if !digestRow.LatestSummary.Valid || strings.TrimSpace(digestRow.LatestSummary.String) == "" {
		return 0, fmt.Errorf("facts: session %s has no summary — run `aichronicles summarize %s` first",
			sessionID, sessionID)
	}

	digest := prompts.SessionDigest{
		ID:      digestRow.ID,
		Summary: digestRow.LatestSummary.String,
	}
	if digestRow.StartedAtMs.Valid {
		digest.StartedAtMs = digestRow.StartedAtMs.Int64
	}
	if digestRow.EndedAtMs.Valid {
		digest.EndedAtMs = digestRow.EndedAtMs.Int64
	}
	if digestRow.Cwd.Valid {
		digest.Cwd = digestRow.Cwd.String
	}
	if digestRow.FirstPrompt.Valid {
		digest.FirstPrompt = digestRow.FirstPrompt.String
	}
	urls, err := store.LoadExtractionsForSession(ctx, s.DB(), sessionID, "url")
	if err != nil {
		return 0, fmt.Errorf("facts: load urls: %w", err)
	}
	if len(urls) > 0 {
		digest.Links = make([]string, len(urls))
		for i, u := range urls {
			digest.Links[i] = u.Value
		}
	}
	shells, err := store.LoadExtractionsForSession(ctx, s.DB(), sessionID, "shell_command")
	if err != nil {
		return 0, fmt.Errorf("facts: load shell_command: %w", err)
	}
	if len(shells) > 0 {
		digest.ShellCommands = make([]string, len(shells))
		for i, sc := range shells {
			digest.ShellCommands[i] = sc.Value
		}
	}
	// Outcome cue — same enrichment as workflow / induction. Less
	// important here (facts are about static contracts, not about
	// outcome), but the cue is cheap and the LLM may use it to
	// down-weight facts asserted in failure_likely sessions.
	if outcome, oerr := store.EnsureSessionOutcome(ctx, s.DB(), sessionID); oerr != nil {
		slog.Warn("facts: skipping outcome cue", "session", sessionID, "err", oerr)
	} else {
		digest.Outcome = outcome
	}

	built, err := prompts.BuildFacts(prompts.FactsFromSessionInputs{Digest: digest})
	if err != nil {
		return 0, fmt.Errorf("facts: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("facts: egress redaction fired",
			"session_id", sessionID,
			"patterns", strings.Join(built.Patterns, ","))
	}

	id, err := runCachedLLM(ctx, c, newClient, cachedLLMInput{
		kind:      store.LLMKindFacts,
		toolName:  prompts.ToolNameFacts,
		result:    new(prompts.FactsResult),
		hash:      built.Hash,
		req:       built.Request,
		model:     opts.Model,
		force:     opts.Force,
		jsonRaw:   opts.JSON,
		output:    io.Discard,
		sessionID: sessionID,
	})
	if err != nil {
		return 0, err
	}

	// Re-load the persisted body and project each fact into
	// semantic_facts. The projection is best-effort per-fact: a
	// single bad fact doesn't drop the whole batch — we log and
	// continue so partial extraction still lands.
	row, err := store.LoadLLMOutputByID(ctx, s.DB(), id)
	if err != nil || row == nil {
		return id, fmt.Errorf("facts: load persisted row: %w", err)
	}
	var result prompts.FactsResult
	if err := json.Unmarshal([]byte(row.Body), &result); err != nil {
		return id, fmt.Errorf("facts: parse persisted body: %w", err)
	}
	persistedCount := persistInducedFacts(ctx, s, id, sessionID, &result)

	if opts.JSON {
		_, _ = io.WriteString(out, row.Body)
		if !strings.HasSuffix(row.Body, "\n") {
			_, _ = io.WriteString(out, "\n")
		}
		return id, nil
	}
	renderFactsResult(out, sessionID, &result, persistedCount)
	return id, nil
}

// persistInducedFacts writes each emitted fact into the
// semantic_facts table. Returns the count actually persisted —
// typically equals len(result.Facts) but may be lower if individual
// rows fail validation. Uses asserted_at_ms = now() so re-runs
// against the same session refresh the timestamp.
func persistInducedFacts(ctx context.Context, s *store.Store, llmOutputID int64, sessionID string, result *prompts.FactsResult) int {
	if result == nil || !result.Found || len(result.Facts) == 0 {
		return 0
	}
	now := time.Now().UnixMilli()
	persisted := 0
	for _, f := range result.Facts {
		if _, err := store.SaveSemanticFact(ctx, s.DB(), store.SemanticFact{
			SourceLLMOutputID: llmOutputID,
			Subject:           f.Subject,
			Predicate:         f.Predicate,
			Object:            f.Object,
			Confidence:        f.Confidence,
			EvidenceSessionID: sql.NullString{String: sessionID, Valid: sessionID != ""},
			EvidenceQuote:     sql.NullString{String: f.Quote, Valid: f.Quote != ""},
			AssertedAtMs:      now,
		}); err != nil {
			slog.Warn("facts: failed to persist induced fact",
				"llm_output_id", llmOutputID,
				"subject", f.Subject,
				"predicate", f.Predicate,
				"object", f.Object,
				"err", err)
			continue
		}
		persisted++
	}
	return persisted
}

// renderFactsResult writes a one-screen summary of the induction
// outcome. Mirrors renderWorkflowResult's shape.
func renderFactsResult(out io.Writer, sessionID string, r *prompts.FactsResult, persisted int) {
	if !r.Found || len(r.Facts) == 0 {
		_, _ = fmt.Fprintf(out, "facts: ✓ %s — no facts\n", sessionID[:8])
		if r.Rationale != "" {
			_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
		}
		return
	}
	_, _ = fmt.Fprintf(out, "facts: ✓ %s — %d fact(s) (%d persisted to semantic_facts)\n",
		sessionID[:8], len(r.Facts), persisted)
	for _, f := range r.Facts {
		_, _ = fmt.Fprintf(out, "  • %s %s = %s  (conf=%.2f)\n",
			f.Subject, f.Predicate, f.Object, f.Confidence)
		if f.Quote != "" {
			_, _ = fmt.Fprintf(out, "    quote: %s\n", f.Quote)
		}
	}
	if r.Rationale != "" {
		_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
	}
}

// renderFactsList renders the LLM-output history for kind=facts.
// Mirrors renderInductionList / renderWorkflowList. Reads the wire
// shape since `facts list` goes through the api now.
func renderFactsList(out io.Writer, rows []api.LLMOutput, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "no facts inductions recorded yet — try `aichronicles facts induce --session <id>`\n")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s  %-19s  %s\n", "session", "when", "outcome")
	fmt.Fprintln(&b, strings.Repeat("-", 80))
	for _, r := range rows {
		var result prompts.FactsResult
		outcome := "(unparseable body)"
		if jerr := json.Unmarshal([]byte(r.Body), &result); jerr == nil {
			if !result.Found || len(result.Facts) == 0 {
				outcome = "no facts — " + result.Rationale
			} else {
				outcome = fmt.Sprintf("%d fact(s) — %s", len(result.Facts),
					truncateForList(result.Rationale, 50))
			}
		}
		sessShort := "(none)"
		if r.SessionID != nil && len(*r.SessionID) >= 8 {
			sessShort = (*r.SessionID)[:8]
		}
		when := time.UnixMilli(r.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC")
		fmt.Fprintf(&b, "%-8s  %-19s  %s\n", sessShort, when, outcome)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// renderFactsForSubject renders every semantic_facts row for one
// subject. Tabular format keyed on predicate so a reader scans
// "what do I know about /work/foo" cleanly.
func renderFactsForSubject(out io.Writer, subject string, facts []api.SemanticFact, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(facts)
	}
	if len(facts) == 0 {
		_, _ = fmt.Fprintf(out, "no facts known for subject %q yet — try `aichronicles facts induce --session <id>` on a session in this project\n", subject)
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "subject: %s\n\n", subject)
	for _, f := range facts {
		fmt.Fprintf(&b, "  %s = %s  (conf=%.2f, asserted %s)\n",
			f.Predicate, f.Object, f.Confidence,
			time.UnixMilli(f.AssertedAtMs).UTC().Format("2006-01-02 15:04 UTC"))
		if f.EvidenceQuote != nil && *f.EvidenceQuote != "" {
			fmt.Fprintf(&b, "    quote: %s\n", *f.EvidenceQuote)
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}
