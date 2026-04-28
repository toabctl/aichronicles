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

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "AWM-style abstract procedural memory induced from a session",
		Long: "Workflows are deliberately ABSTRACT procedural recipes —\n" +
			"drop concrete URLs / IDs / file paths, keep the procedure\n" +
			"shape — that live in the database for retrieval at task-\n" +
			"planning time. Distinct from skills (which produce\n" +
			"~/.claude/skills/<name>/SKILL.md artefacts on disk):\n" +
			"workflows are looser exemplars, retrieved as guidance\n" +
			"rather than applied as installable capabilities.\n\n" +
			"Subcommands:\n" +
			"  induce — induce a workflow from one specific session id\n" +
			"  list   — show recent workflow rows recorded so far\n",
	}
	cmd.AddCommand(newWorkflowInduceCmd())
	cmd.AddCommand(newWorkflowListCmd())
	return cmd
}

func newWorkflowInduceCmd() *cobra.Command {
	var (
		session  string
		model    string
		force    bool
		dbPath   string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "induce --session <id>",
		Short: "Induce an abstract workflow from one specific session",
		Long: "Asks the LLM whether the named session contained a reusable\n" +
			"procedural recipe worth saving as an abstract workflow.\n" +
			"Procedures must drop concrete identifiers — the same task\n" +
			"shape across different projects/repos/tickets must be\n" +
			"recognisable in the task_shape field.\n\n" +
			"The session must have been summarized first. Cached on\n" +
			"(session_id, prompt-hash); --force re-calls the LLM.\n\n" +
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

			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)
			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout))
			defer cancel()

			_, err = RunWorkflowForSession(ctx, s,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				WorkflowRunOptions{
					SessionID: session,
					Model:     model,
					Force:     force,
					JSON:      format == FormatJSON,
				}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id (full or unique prefix) to induce a workflow from")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the cache and re-call the LLM")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newWorkflowListCmd() *cobra.Command {
	var (
		dbPath   string
		limit    int
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show recent workflow inductions (found and not-found verdicts)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			rows, err := store.LoadLLMOutputs(cmd.Context(), s.DB(), store.LLMOutputFilter{
				Kind:  store.LLMKindWorkflow,
				Limit: limit,
			})
			if err != nil {
				return fmt.Errorf("load workflow rows: %w", err)
			}
			return renderWorkflowList(cmd.OutOrStdout(), rows, format)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to render, newest first")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// WorkflowRunOptions drives RunWorkflowForSession.
type WorkflowRunOptions struct {
	SessionID string
	Model     string
	Force     bool
	JSON      bool
}

// RunWorkflowForSession orchestrates single-session workflow
// induction: resolve session prefix → load digest → require summary
// → enrich with extractions and outcome → BuildWorkflow → cached LLM
// → render. Returns the persisted llm_outputs row id.
//
// The session must have been summarized first; without a summary
// the digest is too thin to ground an abstract procedure. Returns
// a clear error in that case rather than emitting a low-quality
// workflow row.
func RunWorkflowForSession(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts WorkflowRunOptions,
	out io.Writer,
) (int64, error) {
	sessionID, err := store.ResolveSessionIDPrefix(ctx, s.DB(), opts.SessionID)
	if err != nil {
		return 0, fmt.Errorf("workflow: %w", err)
	}

	digestRow, err := store.LoadSessionDigest(ctx, s.DB(), sessionID)
	if err != nil {
		return 0, fmt.Errorf("workflow: load digest: %w", err)
	}
	if digestRow == nil {
		return 0, fmt.Errorf("workflow: session %s not found", sessionID)
	}
	if !digestRow.LatestSummary.Valid || strings.TrimSpace(digestRow.LatestSummary.String) == "" {
		return 0, fmt.Errorf("workflow: session %s has no summary — run `aichronicles summarize %s` first",
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
		return 0, fmt.Errorf("workflow: load urls: %w", err)
	}
	if len(urls) > 0 {
		digest.Links = make([]string, len(urls))
		for i, u := range urls {
			digest.Links[i] = u.Value
		}
	}
	shells, err := store.LoadExtractionsForSession(ctx, s.DB(), sessionID, "shell_command")
	if err != nil {
		return 0, fmt.Errorf("workflow: load shell_command: %w", err)
	}
	if len(shells) > 0 {
		digest.ShellCommands = make([]string, len(shells))
		for i, sc := range shells {
			digest.ShellCommands[i] = sc.Value
		}
	}

	// Outcome enrichment — same pattern as RunInductionForSession.
	// failure_likely sessions tend not to ground a workflow, so
	// surfacing the cue helps the LLM bias correctly.
	if outcome, oerr := store.EnsureSessionOutcome(ctx, s.DB(), sessionID); oerr != nil {
		slog.Warn("workflow: skipping outcome cue", "session", sessionID, "err", oerr)
	} else {
		digest.Outcome = outcome
	}

	built, err := prompts.BuildWorkflow(prompts.WorkflowFromSessionInputs{
		Digest: digest,
	})
	if err != nil {
		return 0, fmt.Errorf("workflow: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("workflow: egress redaction fired",
			"session_id", sessionID,
			"patterns", strings.Join(built.Patterns, ","))
	}

	id, err := runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:      store.LLMKindWorkflow,
		toolName:  prompts.ToolNameWorkflow,
		result:    new(prompts.WorkflowResult),
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

	row, err := store.LoadLLMOutputByID(ctx, s.DB(), id)
	if err != nil || row == nil {
		return id, fmt.Errorf("workflow: load persisted row: %w", err)
	}
	if opts.JSON {
		_, _ = io.WriteString(out, row.Body)
		if !strings.HasSuffix(row.Body, "\n") {
			_, _ = io.WriteString(out, "\n")
		}
		return id, nil
	}
	var result prompts.WorkflowResult
	if err := json.Unmarshal([]byte(row.Body), &result); err != nil {
		return id, fmt.Errorf("workflow: parse persisted body: %w", err)
	}
	renderWorkflowResult(out, sessionID, &result)
	return id, nil
}

// renderWorkflowResult writes a one-screen render of a workflow
// induction. Distinct paths for "no workflow found" vs a found
// workflow (where we surface task_shape + numbered procedure).
func renderWorkflowResult(out io.Writer, sessionID string, r *prompts.WorkflowResult) {
	if !r.Found {
		_, _ = fmt.Fprintf(out, "workflow: ✓ %s — no workflow\n", sessionID[:8])
		if r.Rationale != "" {
			_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
		}
		return
	}
	_, _ = fmt.Fprintf(out, "workflow: ✓ %s — %q\n", sessionID[:8], r.TaskShape)
	if len(r.Preconditions) > 0 {
		_, _ = fmt.Fprintln(out, "  preconditions:")
		for _, p := range r.Preconditions {
			_, _ = fmt.Fprintf(out, "    - %s\n", p)
		}
	}
	if len(r.Procedure) > 0 {
		_, _ = fmt.Fprintln(out, "  procedure:")
		for i, step := range r.Procedure {
			_, _ = fmt.Fprintf(out, "    %d. %s\n", i+1, step.Action)
			for _, p := range step.Placeholders {
				ex := ""
				if p.Example != "" {
					ex = "  e.g. " + p.Example
				}
				_, _ = fmt.Fprintf(out, "       {%s} — %s%s\n", p.Token, p.Description, ex)
			}
		}
	}
	if len(r.SuccessChecks) > 0 {
		_, _ = fmt.Fprintln(out, "  success checks:")
		for _, c := range r.SuccessChecks {
			_, _ = fmt.Fprintf(out, "    - %s\n", c)
		}
	}
	if r.Rationale != "" {
		_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
	}
}

// renderWorkflowList renders the workflow history — one row per
// llm_outputs row of kind=workflow, newest first. task_shape is the
// found workflow's shape, OR "(no workflow)" when the verdict was
// negative.
func renderWorkflowList(out io.Writer, rows []store.LLMOutput, format OutputFormat) error {
	if format == FormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "no workflow inductions recorded yet — try `aichronicles workflow induce --session <id>`\n")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s  %-19s  %s\n", "session", "when", "shape")
	fmt.Fprintln(&b, strings.Repeat("-", 80))
	for _, r := range rows {
		var result prompts.WorkflowResult
		shape := "(unparseable body)"
		if jerr := json.Unmarshal([]byte(r.Body), &result); jerr == nil {
			if !result.Found {
				shape = "no workflow — " + result.Rationale
			} else {
				shape = result.TaskShape
			}
		}
		sessShort := "(none)"
		if r.SessionID.Valid && len(r.SessionID.String) >= 8 {
			sessShort = r.SessionID.String[:8]
		}
		when := time.UnixMilli(r.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC")
		fmt.Fprintf(&b, "%-8s  %-19s  %s\n", sessShort, when, truncateForList(shape, 60))
	}
	_, err := io.WriteString(out, b.String())
	return err
}
