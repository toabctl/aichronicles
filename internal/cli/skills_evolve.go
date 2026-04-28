package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

const defaultEvolveSinceWindow = 30 * 24 * time.Hour
const defaultEvolveExamples = 5

// newSkillsEvolveCmd is the third sibling under `aichronicles
// skills` (alongside stale + impact). The staleness detector flags
// skills whose loads correlate with tool_failure events; this
// command actually does something with the flag — drafts a revised
// SKILL.md grounded in the captured failure context.
func newSkillsEvolveCmd() *cobra.Command {
	var (
		skillName   string
		skillsDir   string
		dbPath      string
		since       time.Duration
		window      time.Duration
		maxExamples int
		force       bool
		model       string
	)
	cmd := &cobra.Command{
		Use:   "evolve --skill <name>",
		Short: "Draft a revised SKILL.md for a stale-correlated skill, grounded in captured failures",
		Long: "Reads ~/.claude/skills/<name>/SKILL.md and the failure events\n" +
			"the staleness detector found correlated with this skill, then\n" +
			"asks the LLM to revise the SKILL: tighten the trigger, add\n" +
			"pitfalls, fix concrete instruction errors. The frontmatter is\n" +
			"preserved verbatim — the SKILL keeps its identity.\n\n" +
			"Output lands at ~/.claude/skills/<name>/SKILL.md.v2 — the\n" +
			"original is left alone. Diff the two and replace manually if\n" +
			"the revision looks good. The LLM may also decide no revision\n" +
			"is warranted (failures look unrelated, evidence too thin) and\n" +
			"return a no-change verdict instead.\n\n" +
			"Implements gap #4 from the research-vs-aichronicles comparison\n" +
			"memory: the TDS Voyager critique notes that Voyager-style\n" +
			"systems flag stale skills but rarely act on them — this\n" +
			"command is the act-on-it side.\n\n" +
			"Cached under kind=skill_revision keyed on the SKILL's\n" +
			"content-hash + name, so re-running on an unchanged SKILL is\n" +
			"free; hand-editing the SKILL invalidates the cache.\n\n" +
			"Requires the LLM provider to be configured (same as\n" +
			"summarize/reflect/propose).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if skillName == "" {
				return errors.New("--skill <name> is required (run `aichronicles skills stale` to find candidates)")
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
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()
			newClient := func() (llm.Client, error) {
				return llm.FromConfig(ctx, llmCfg)
			}

			return runSkillsEvolve(ctx, s, newClient, skillsEvolveOptions{
				SkillName:   skillName,
				SkillsDir:   resolveSkillsDir(skillsDir),
				Since:       since,
				Window:      window,
				MaxExamples: maxExamples,
				Force:       force,
				Model:       model,
			}, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&skillName, "skill", "", "name of the SKILL to revise (must exist under --skills-dir)")
	cmd.Flags().StringVar(&skillsDir, "skills-dir", "", "override target directory (default: ~/.claude/skills)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addFlexDurationFlag(cmd, &since, "since", defaultEvolveSinceWindow,
		"only consider failures within this window (e.g. 14d, 30d)")
	addFlexDurationFlag(cmd, &window, "window", defaultSkillStaleWindow,
		"how long after a skill load to look for a tool_failure (e.g. 5m, 10m)")
	cmd.Flags().IntVar(&maxExamples, "examples", defaultEvolveExamples,
		"how many failure examples to include in the prompt")
	cmd.Flags().BoolVar(&force, "force", false,
		"bypass the cache and re-call the LLM even if a revision was already drafted")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	return cmd
}

type skillsEvolveOptions struct {
	SkillName   string
	SkillsDir   string
	Since       time.Duration
	Window      time.Duration
	MaxExamples int
	Force       bool
	Model       string
}

// SkillsEvolveOptions drives RunSkillsEvolve. Exported so the
// daemon's meta-analysis sweeper (and external test callers)
// can drive the same code path as the CLI command.
type SkillsEvolveOptions = skillsEvolveOptions

// RunSkillsEvolve drafts a revised SKILL.md for one skill name,
// grounded in the captured failure events the staleness detector
// flagged. Reads ~/.claude/skills/<name>/SKILL.md, persists the
// revision under llm_outputs(kind=skill_revision), writes the
// revised body to ~/.claude/skills/<name>/SKILL.md.v2.
//
// Cache-idempotent on (skill name, current SKILL.md body, failure
// context) — re-running on an unchanged SKILL hits the cache for
// free.
func RunSkillsEvolve(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts SkillsEvolveOptions,
	out, errOut io.Writer,
) error {
	return runSkillsEvolve(ctx, s, newClient, opts, out, errOut)
}

func runSkillsEvolve(
	ctx context.Context,
	s *store.Store,
	newClient func() (llm.Client, error),
	opts skillsEvolveOptions,
	out, errOut io.Writer,
) error {
	since := opts.Since
	if since <= 0 {
		since = defaultEvolveSinceWindow
	}
	window := opts.Window
	if window <= 0 {
		window = defaultSkillStaleWindow
	}
	maxExamples := opts.MaxExamples
	if maxExamples <= 0 {
		maxExamples = defaultEvolveExamples
	}

	// Locate the SKILL.md on disk.
	skillPath := filepath.Join(opts.SkillsDir, opts.SkillName, "SKILL.md")
	skillBody, err := os.ReadFile(skillPath)
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}

	// Pull failure context from the store.
	sinceMs := time.Now().Add(-since).UnixMilli()
	failures, err := store.LoadSkillFailures(ctx, s.DB(),
		opts.SkillName, sinceMs, window.Milliseconds(), maxExamples)
	if err != nil {
		return fmt.Errorf("load failures: %w", err)
	}

	_, _ = fmt.Fprintf(errOut,
		"evolve: skill=%s  failures_found=%d  since=%s  window=%s\n",
		opts.SkillName, len(failures), humanDuration(since), humanDuration(window))
	if len(failures) == 0 {
		_, _ = fmt.Fprintf(errOut,
			"  no failure evidence in window — nothing to evolve from. "+
				"Consider widening --since or running `aichronicles skills stale` "+
				"to confirm there are correlated failures.\n")
		return nil
	}

	examples := make([]prompts.SkillFailureExample, 0, len(failures))
	for _, f := range failures {
		ctxText := f.FailBody
		if f.NearbyText != "" {
			ctxText = "FAIL_BODY:\n" + f.FailBody + "\n\nNEARBY EVENTS:\n" + f.NearbyText
		}
		examples = append(examples, prompts.SkillFailureExample{
			SessionID:      f.SessionID,
			TsMs:           f.FailTsMs,
			ContextSnippet: ctxText,
		})
	}

	built, err := prompts.BuildEvolveSkill(prompts.EvolveSkillInputs{
		SkillName:       opts.SkillName,
		CurrentSkillMd:  string(skillBody),
		FailureExamples: examples,
	})
	if err != nil {
		return fmt.Errorf("build evolve prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("skills evolve: egress redaction fired",
			"patterns", strings.Join(built.Patterns, ","))
	}

	// Cache hash: built.Hash already factors in skill name +
	// SKILL.md body via the prompt text, so two runs on the same
	// SKILL hit the cache for free; a hand-edit invalidates it.
	id, err := runCachedLLM(ctx, s, newClient, cachedLLMInput{
		kind:     store.LLMKindSkillRevision,
		toolName: prompts.ToolNameSkillRevision,
		result:   new(prompts.SkillRevision),
		hash:     built.Hash,
		req:      built.Request,
		model:    opts.Model,
		force:    opts.Force,
		jsonRaw:  true, // we render below; suppress the default emitter
		output:   io.Discard,
	})
	if err != nil {
		return err
	}

	// Re-load the persisted body so we render exactly what was
	// stored (cache-hit and cache-miss go through the same path).
	row, err := store.LoadLLMOutputByID(ctx, s.DB(), id)
	if err != nil || row == nil {
		return fmt.Errorf("load persisted revision: %w", err)
	}
	var revision prompts.SkillRevision
	if err := json.Unmarshal([]byte(row.Body), &revision); err != nil {
		return fmt.Errorf("parse persisted revision: %w", err)
	}

	if revision.NoChangeNeeded {
		_, _ = fmt.Fprintf(out,
			"evolve: ✓ no revision needed — %s\n",
			revision.Rationale)
		return nil
	}

	v2Path := skillPath + ".v2"
	if err := os.WriteFile(v2Path, []byte(revision.RevisedBody), 0o644); err != nil {
		return fmt.Errorf("write v2: %w", err)
	}
	_, _ = fmt.Fprintf(out, "evolve: ✓ wrote %s\n", v2Path)
	_, _ = fmt.Fprintf(out, "  rationale: %s\n", revision.Rationale)
	_, _ = fmt.Fprintf(out, "  diff: diff %s %s\n", skillPath, v2Path)
	_, _ = fmt.Fprintf(out, "  apply: mv %s %s  (after reviewing the diff)\n", v2Path, skillPath)
	return nil
}
