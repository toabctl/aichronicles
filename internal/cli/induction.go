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
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/textfmt"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultInductionIdle re-exports store.DefaultInductionIdle as
// the local sweep flag's default so cli and store can't drift.
// 30 minutes is comfortable: long enough that an active conversation
// pausing for a coffee break doesn't trip the sweeper, short enough
// that a finished session gets processed while it's still
// cognitively warm for the user.
const defaultInductionIdle = store.DefaultInductionIdle

// defaultInductionMinEvents drops trivially-short sessions ("user
// typed `q` and bailed") from the sweep. 5 events is one
// user_prompt + one assistant_message + a tool_use round-trip;
// anything below that won't ground a workflow.
const defaultInductionMinEvents = 5

// defaultInductionLimit caps how many sessions one sweep
// processes. With ~50¢/run typical and the session-level cache
// keeping re-runs free, a manual `induction sweep` walking 25
// at a time is the right blast radius.
const defaultInductionLimit = 25

func newInductionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "induction",
		Short: "Online single-session induction (AWM-style auto-skill-extraction)",
		Long: "When a session ends, induction asks the LLM whether the\n" +
			"trajectory contained ONE concrete reusable workflow worth\n" +
			"saving as a Claude Code skill. The bar is high — the model\n" +
			"is told to default to no_skill_found unless it can name a\n" +
			"specific trigger condition the user is likely to hit again.\n\n" +
			"Subcommands:\n" +
			"  sweep — walk recently-ended sessions and induce on each\n" +
			"  run   — induce on one specific session id\n" +
			"  list  — show induction outcomes recorded so far\n",
	}
	cmd.AddCommand(newInductionSweepCmd())
	cmd.AddCommand(newInductionRunCmd())
	cmd.AddCommand(newInductionListCmd())
	return cmd
}

func newInductionSweepCmd() *cobra.Command {
	var (
		idle          time.Duration
		minEvents     int
		limit         int
		model         string
		dbPath        string
		sockFlag      string
		skipSummarize bool
		skipFacts     bool
		skipEpisodes  bool
	)
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Walk recently-ended sessions and run the auto-extraction pipeline on each",
		Long: "Selects sessions that (a) have an ended_at_ms older than\n" +
			"--idle, (b) have at least --min-events recorded events,\n" +
			"and (c) haven't been induced before, then runs the per-\n" +
			"session pipeline on each:\n\n" +
			"  phase 1: summarize       (when no kind=summary row exists)\n" +
			"  phase 2: induction       (skill + workflow merged)\n" +
			"  phase 3: facts           (typed semantic facts)\n\n" +
			"Each phase is cache-idempotent on prompt-hash — re-running\n" +
			"the sweep skips sessions whose rows already exist. Phase 1\n" +
			"failure SKIPS phases 2+3 (they require a summary); phases\n" +
			"2 and 3 are independent and run even if the other failed.\n\n" +
			"Designed to be triggered from the daemon's resident\n" +
			"sweeper. The CLI prints a per-session line so the\n" +
			"operator can tell which session yielded a skill, a\n" +
			"workflow, both, or nothing.\n\n" +
			"Pass --skip-summarize to keep summarize manual (sessions\n" +
			"without summaries will then bail with their existing 'no\n" +
			"summary' error in phase 2). Pass --skip-facts to suppress\n" +
			"the facts induction call. Either flag halves per-\n" +
			"candidate spend.\n\n" +
			"Requires " + llm.APIKeyEnv + ".",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			// No outer WithTimeout: each per-phase LLM call inside
			// RunInductionSweep gets its own bounded context. A
			// single sweep-level timeout would strangle every call
			// after the budget elapses, regardless of which session
			// is in flight — the bug session 9ec75b11 tripped.
			ctx := cmd.Context()
			return RunInductionSweep(ctx, s, c,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				InductionSweepOptions{
					Idle:             idle,
					MinEvents:        minEvents,
					Limit:            limit,
					Model:            model,
					SkipSummarize:    skipSummarize,
					SkipFacts:        skipFacts,
					SkipEpisodes:     skipEpisodes,
					SummarizeTimeout: cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout),
					InductionTimeout: cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout),
					FactsTimeout:     cfg.Limits.SummarizeTimeout.Or(defaultSummarizeTimeout),
				}, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addFlexDurationFlag(cmd, &idle, "idle", defaultInductionIdle,
		"only consider sessions with no events for this long (e.g. 15m, 1h)")
	cmd.Flags().IntVar(&minEvents, "min-events", defaultInductionMinEvents,
		"skip sessions with fewer than this many events")
	cmd.Flags().IntVar(&limit, "limit", defaultInductionLimit,
		"max sessions to process in one sweep")
	addModelFlag(cmd, &model)
	addDBFlag(cmd, &dbPath)
	cmd.Flags().BoolVar(&skipSummarize, "skip-summarize", false,
		"skip phase 1 (auto-summarize). Sessions without summaries will be skipped — keeps summarize manual.")
	cmd.Flags().BoolVar(&skipFacts, "skip-facts", false,
		"skip phase 3 (semantic-facts induction); saves one LLM call per candidate")
	cmd.Flags().BoolVar(&skipEpisodes, "skip-episodes", false,
		"skip phase 0 (episode segmentation); episode-keyed retrieval will have no rows for new candidates")
	addSocketFlag(cmd, &sockFlag)
	return cmd
}

func newInductionRunCmd() *cobra.Command {
	var (
		session  string
		model    string
		force    bool
		dbPath   string
		sockFlag string
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "run --session <id>",
		Short: "Induce on one specific session id",
		Long: "Same prompt and persistence as `induction sweep`, but for a\n" +
			"single session you name explicitly. Useful for replaying or\n" +
			"force-recomputing — pair with --force to bypass the cache.\n\n" +
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

			_, err = RunInductionForSession(ctx, s, c,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				InductionRunOptions{
					SessionID: session,
					Model:     model,
					Force:     force,
					JSON:      format == FormatJSON,
				}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id (full or unique prefix) to induce on")
	addModelFlag(cmd, &model)
	addForceLLMCacheFlag(cmd, &force)
	addDBFlag(cmd, &dbPath)
	addFormatFlag(cmd, &formatIn)
	return cmd
}

func newInductionListCmd() *cobra.Command {
	var (
		sockFlag string
		limit    int
		formatIn string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show recent induction runs (proposed skills, no_skill_found verdicts)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := ParseOutputFormat(formatIn)
			if err != nil {
				return err
			}
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}

			rows, err := c.LLMOutputsList(cmd.Context(),
				string(store.LLMKindInduction), "", limit)
			if err != nil {
				return fmt.Errorf("load induction rows: %w", err)
			}
			return renderInductionList(cmd.OutOrStdout(), rows, format)
		},
	}
	addSocketFlag(cmd, &sockFlag)
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to render, newest first")
	addFormatFlag(cmd, &formatIn)
	return cmd
}

// InductionSweepOptions drives RunInductionSweep.
type InductionSweepOptions struct {
	Idle      time.Duration
	MinEvents int
	Limit     int
	Model     string

	// SkipSummarize, when true, suppresses the phase-1 summarize
	// call that the sweep otherwise fires for sessions lacking a
	// kind=summary llm_outputs row. Default (SkipSummarize=false)
	// is "auto-summarize then auto-extract" — the autonomous
	// pipeline. Set true to keep summarize manual; downstream
	// phases (induction / facts) will then bail with their
	// existing "no summary" error for sessions you haven't
	// summarized by hand.
	SkipSummarize bool

	// SkipFacts, when true, suppresses the per-candidate
	// facts-induction LLM call that the sweep otherwise fires
	// alongside the (skill+workflow merged) induction call.
	// Default (SkipFacts=false) is "auto-extract every memory
	// type from a settled session". Set true at the cost of an
	// empty semantic-facts table.
	SkipFacts bool

	// SkipEpisodes, when true, suppresses the per-candidate
	// episode segmentation phase. Default (SkipEpisodes=false)
	// runs SegmentSession + SaveEpisodes for every candidate as
	// a cheap local-only step (no LLM call) so episode-keyed
	// retrieval has data to work with. Pink et al. (2026 —
	// arXiv:2502.06975) treats segmentation as the substrate for
	// instance-specific recall; without it the daemon ingests
	// events but never produces the episode index. Set true if
	// you want to defer episode persistence to manual control.
	SkipEpisodes bool

	// SummarizeTimeout caps a single summarize LLM round-trip
	// inside the sweep. Per-call (not per-sweep) so one slow
	// session can't strangle the next 399. Zero falls back to
	// defaultSummarizeTimeout (3m). Same semantics for FactsTimeout.
	SummarizeTimeout time.Duration

	// InductionTimeout caps a single induction LLM round-trip.
	// Zero falls back to defaultMetaLLMTimeout (5m).
	InductionTimeout time.Duration

	// FactsTimeout caps a single facts LLM round-trip. Zero falls
	// back to defaultSummarizeTimeout (3m); facts prompts are the
	// same shape as summarize so the same budget applies.
	FactsTimeout time.Duration
}

// RunInductionSweep walks idle sessions and runs RunInductionForSession
// on each. Errors on any one session are logged but don't abort the
// sweep — one bad session shouldn't block the rest of the queue.
//
// Returns nil iff every candidate either (a) produced an induction
// row or (b) was already cached. A wrapped error returns when no
// candidates exist (typical case for a fresh DB) so a cron-driven
// caller can detect "nothing to do" cleanly.
func RunInductionSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts InductionSweepOptions,
	out, errOut io.Writer,
) error {
	idle := opts.Idle
	if idle <= 0 {
		idle = defaultInductionIdle
	}
	minEvents := opts.MinEvents
	if minEvents <= 0 {
		minEvents = defaultInductionMinEvents
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultInductionLimit
	}

	// Phase 0 (sweep-wide): segment any session whose episodes
	// table is stale relative to its events table. This pass runs
	// independently of induction state — once a session is
	// inducted it permanently drops out of the candidate set, so
	// late-arriving events would never get re-segmented if phase 0
	// stayed inside the per-candidate loop. The sweep-wide pass
	// keys on episode staleness directly: missing episodes OR an
	// event newer than the latest episode's ended_at_ms.
	//
	// Pure local work (no LLM); idempotent via DELETE-then-INSERT
	// in SaveEpisodes, so a session that got picked up here AND is
	// also an induction candidate below produces the same episodes
	// either way. We use a generous limit (4× induction's) so
	// segmentation isn't the bottleneck — the operation is cheap
	// and segmentation lag is the foundation everything downstream
	// relies on.
	if !opts.SkipEpisodes {
		segLimit := limit * 4
		if segLimit < 50 {
			segLimit = 50
		}
		segResp, serr := c.SessionsNeedingSegmentation(ctx, wire.SessionsNeedingSegmentationRequest{
			IdleCutoffMs: time.Now().UnixMilli(),
			IdleMs:       idle.Milliseconds(),
			MinEvents:    minEvents,
			Limit:        segLimit,
		})
		if serr != nil {
			slog.Warn("induction sweep: failed to load sessions needing segmentation", "err", serr)
		} else if len(segResp.SessionIDs) > 0 {
			_, _ = fmt.Fprintf(errOut, "induction sweep: segmenting %d session(s)\n", len(segResp.SessionIDs))
			for _, sid := range segResp.SessionIDs {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if _, eerr := c.SegmentSession(ctx, sid, wire.SegmentSessionRequest{}); eerr != nil {
					slog.Warn("induction sweep: episode segmentation failed",
						"session_id", sid, "err", eerr)
					_, _ = fmt.Fprintf(errOut, "  ✗ episodes %s: %v\n", preview.ShortID(sid), eerr)
				}
			}
		}
	}

	candResp, err := c.InductionCandidates(ctx, wire.InductionCandidatesRequest{
		NowMs:           time.Now().UnixMilli(),
		IdleThresholdMs: idle.Milliseconds(),
		MinEvents:       minEvents,
		Limit:           limit,
	})
	if err != nil {
		return fmt.Errorf("load induction candidates: %w", err)
	}
	candidates := candResp.Candidates
	_, _ = fmt.Fprintf(errOut,
		"induction sweep: idle=%s  min_events=%d  candidates=%d\n",
		humanDuration(idle), minEvents, len(candidates))
	if len(candidates) == 0 {
		_, _ = fmt.Fprintf(out, "induction sweep: nothing to do (no idle, un-induced sessions in window)\n")
		return nil
	}

	// Per-phase timeout budgets. Each LLM call inside the loop
	// gets its own bounded context derived from the parent ctx
	// — so the parent ctx stays the cancellation channel (Ctrl-C
	// or daemon shutdown), while each call gets a clean per-call
	// budget. Without this, a multi-session sweep that inherits
	// a single parent timeout would strangle every call after
	// the first slow one with "context deadline exceeded" — the
	// exact bug session 9ec75b11's facts phase tripped.
	summarizeTO := opts.SummarizeTimeout
	if summarizeTO <= 0 {
		summarizeTO = defaultSummarizeTimeout
	}
	inductionTO := opts.InductionTimeout
	if inductionTO <= 0 {
		inductionTO = defaultMetaLLMTimeout
	}
	factsTO := opts.FactsTimeout
	if factsTO <= 0 {
		factsTO = defaultSummarizeTimeout
	}

	for _, cand := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// (Phase 0 — episode segmentation — moved out of this loop
		// so it also covers already-inducted sessions. See the
		// sweep-wide pass at the top of RunInductionSweep.)

		// Phase 1: auto-summarize when no kind=summary row exists
		// for this session yet. Closes the manual gate that used
		// to block phases 2+3 — the autonomous pipeline.
		//
		// Failure on phase 1 SKIPS phases 2+3 for this session:
		// induction and facts both gate on summary and would just
		// log "no summary" errors of their own. Next tick retries.
		// summary cache-hits are detected by HasLLMOutputForSession
		// (cheap row-exists check); we only call RunSummarize when
		// the kind=summary row is genuinely missing.
		summaryAvailable := true
		if !opts.SkipSummarize {
			has, herr := c.LLMOutputExistsForSession(ctx, cand.ID, string(store.LLMKindSummary))
			if herr != nil {
				slog.Warn("induction sweep: summary existence check failed",
					"session_id", cand.ID, "err", herr)
				// Best-effort: try downstream phases — they have
				// their own "no summary" branch and will bail with
				// a clear error if needed.
			} else if !has {
				phaseCtx, cancel := context.WithTimeout(ctx, summarizeTO)
				_, serr := RunSummarize(phaseCtx, c, newClient, SummarizeOptions{
					SessionID: cand.ID,
					Model:     opts.Model,
				}, io.Discard)
				cancel()
				if serr != nil {
					slog.Warn("induction sweep: summarize failed",
						"session_id", cand.ID, "err", serr)
					_, _ = fmt.Fprintf(errOut,
						"  ✗ summarize %s: %v\n", preview.ShortID(cand.ID), serr)
					summaryAvailable = false
				}
			}
		}
		if !summaryAvailable {
			// Skip phases 2+3 — they require a summary.
			continue
		}

		// Phase 2: induction (skill+workflow merged after Round 8).
		phaseCtx, cancel := context.WithTimeout(ctx, inductionTO)
		_, err := RunInductionForSession(phaseCtx, s, c, newClient, InductionRunOptions{
			SessionID: cand.ID,
			Model:     opts.Model,
		}, out)
		cancel()
		if err != nil {
			// One session's failure shouldn't kill the sweep — log
			// it and continue. The next sweep retries this session
			// (no induction row was written).
			slog.Warn("induction sweep: session failed",
				"session_id", cand.ID, "err", err)
			_, _ = fmt.Fprintf(errOut,
				"  ✗ %s: %v\n", preview.ShortID(cand.ID), err)
		}

		// Phase 3: auto-extract semantic facts from the same
		// candidate when not opted out. Independent of phase-2
		// success — facts only requires a summary, not an
		// induction row, so it runs even if induction failed.
		if !opts.SkipFacts {
			phaseCtx, cancel := context.WithTimeout(ctx, factsTO)
			_, ferr := RunFactsForSession(phaseCtx, c, newClient, FactsRunOptions{
				SessionID: cand.ID,
				Model:     opts.Model,
			}, io.Discard)
			cancel()
			if ferr != nil {
				slog.Warn("induction sweep: facts failed",
					"session_id", cand.ID, "err", ferr)
				_, _ = fmt.Fprintf(errOut,
					"  ✗ facts %s: %v\n", preview.ShortID(cand.ID), ferr)
			}
		}
	}
	return nil
}

// segmentAndSaveEpisodes used to combine the unbounded event load
// with the segmenter call and the SaveEpisodes persist; the api
// folded that triple into POST /v1/sessions/{id}/segment, so the
// only remaining caller (the induction sweep) inlines
// c.SegmentSession directly. Helper retained as documentation of
// the logical operation; new callers can drop it in favour of the
// one-liner.

// InductionRunOptions drives RunInductionForSession.
type InductionRunOptions struct {
	SessionID string
	Model     string
	Force     bool
	JSON      bool
}

// RunInductionForSession does the single-session induction: build
// digest from session row + summary + extractions, build the
// induction prompt, hit cache or call LLM, persist with
// kind='induction', render result.
//
// Returns the llm_outputs row id on cache hit OR fresh write.
//
// The session needs to have been summarized — without a summary
// the digest is too thin to ground a useful induction. Returns a
// clear error in that case rather than running on a stub digest.
func RunInductionForSession(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts InductionRunOptions,
	out io.Writer,
) (int64, error) {
	sessionID, err := c.ResolveSession(ctx, opts.SessionID)
	if err != nil {
		return 0, fmt.Errorf("induction: %w", err)
	}

	digestRow, err := c.Session(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("induction: load digest: %w", err)
	}
	if digestRow.LatestSummary == nil || strings.TrimSpace(*digestRow.LatestSummary) == "" {
		return 0, fmt.Errorf("induction: session %s has no summary — run `aichronicles summarize %s` first",
			sessionID, sessionID)
	}

	digest := prompts.SessionDigest{
		ID:      digestRow.ID,
		Summary: *digestRow.LatestSummary,
	}
	if digestRow.StartedAtMs != nil {
		digest.StartedAtMs = *digestRow.StartedAtMs
	}
	if digestRow.EndedAtMs != nil {
		digest.EndedAtMs = *digestRow.EndedAtMs
	}
	if digestRow.Cwd != nil {
		digest.Cwd = *digestRow.Cwd
	}
	if digestRow.FirstPrompt != nil {
		digest.FirstPrompt = *digestRow.FirstPrompt
	}
	urls, err := c.SessionExtractions(ctx, sessionID, "url")
	if err != nil {
		return 0, fmt.Errorf("induction: load urls: %w", err)
	}
	if len(urls.Extractions) > 0 {
		digest.Links = make([]string, len(urls.Extractions))
		for i, u := range urls.Extractions {
			digest.Links[i] = u.Value
		}
	}
	shells, err := c.SessionExtractions(ctx, sessionID, "shell_command")
	if err != nil {
		return 0, fmt.Errorf("induction: load shell_command: %w", err)
	}
	if len(shells.Extractions) > 0 {
		digest.ShellCommands = make([]string, len(shells.Extractions))
		for i, sc := range shells.Extractions {
			digest.ShellCommands[i] = sc.Value
		}
	}

	// Outcome enrichment: fill the cached row or compute one. Lets
	// the induction prompt apply its outcome-aware bias (failure_likely
	// → default to no_skill_found unless the failure itself reveals a
	// reusable trigger). Best-effort.
	if outcome, oerr := c.SessionOutcome(ctx, sessionID); oerr != nil {
		slog.Warn("induction: skipping outcome cue", "session", sessionID, "err", oerr)
	} else {
		digest.Outcome = &outcome
	}

	// Installed-skills enrichment so the induction prompt won't
	// repropose a skill that already exists. Best-effort: a
	// failure here downgrades the prompt rather than blocking
	// the run.
	installed, ierr := skills.CollectInstalled(ctx, s.DB(),
		time.Now().Add(-30*24*time.Hour).UnixMilli())
	if ierr != nil {
		slog.Warn("induction: skipping installed-skills enrichment", "err", ierr)
	}

	built, err := prompts.BuildInduce(prompts.InduceFromSessionInputs{
		Digest:          digest,
		InstalledSkills: installed,
	})
	if err != nil {
		return 0, fmt.Errorf("induction: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("induction: egress redaction fired",
			"session_id", sessionID,
			"patterns", strings.Join(built.Patterns, ","))
	}

	id, err := runCachedLLM(ctx, c, newClient, cachedLLMInput{
		kind:      store.LLMKindInduction,
		toolName:  prompts.ToolNameInduction,
		result:    new(prompts.InductionResult),
		hash:      built.Hash,
		req:       built.Request,
		model:     opts.Model,
		force:     opts.Force,
		jsonRaw:   opts.JSON,
		output:    io.Discard, // we render below
		sessionID: sessionID,
	})
	if err != nil {
		return 0, err
	}

	row, err := c.LLMOutputByID(ctx, id)
	if err != nil {
		return id, fmt.Errorf("induction: load persisted row: %w", err)
	}
	var result prompts.InductionResult
	if err := json.Unmarshal([]byte(row.Body), &result); err != nil {
		return id, fmt.Errorf("induction: parse persisted body: %w", err)
	}
	// Anti-fabrication grounding: the induction prompt is grounded
	// in exactly one session (sessionID). Drop every evidence row
	// — on the Skill and on the Workflow — whose SessionID points
	// somewhere else; the LLM cannot legitimately cite another
	// session, so a foreign id is a hallucination. The schema's
	// UUIDv5 pattern already rejects malformed strings; this catches
	// the harder case where the model emits a syntactically valid
	// UUID that isn't the input session's id.
	result.GroundInductionEvidence(sessionID)
	// Lifecycle tracking: if the LLM extracted a skill, record it
	// in skill_candidates with its AutoSkill metadata (triggers,
	// tags, examples, version). A later maintenance decision (add /
	// merge / discard) attributes the resulting SKILL.md back to
	// this induction run, and the candidate survives in the index
	// even if the user never acts on it (the abandonment-rate
	// signal). Best-effort — same pattern as
	// recordSkillCandidatesFromProposal.
	if result.Skill != nil && result.Skill.Name != "" {
		meta := skillMetadataFromProposed(*result.Skill)
		if _, rerr := c.RecordSkillCandidate(ctx, wire.RecordSkillCandidateRequest{
			LLMOutputID:  id,
			SkillName:    result.Skill.Name,
			ProposedAtMs: row.CreatedAtMs,
			Metadata:     meta,
		}); rerr != nil {
			slog.Warn("induction: failed to record skill candidate",
				"llm_output_id", id, "skill", result.Skill.Name, "err", rerr)
		}
	}
	if opts.JSON {
		_, _ = io.WriteString(out, row.Body)
		if !strings.HasSuffix(row.Body, "\n") {
			_, _ = io.WriteString(out, "\n")
		}
		return id, nil
	}
	renderInductionResult(out, sessionID, &result)
	return id, nil
}

// renderInductionResult writes a one-screen summary of the
// induction outcome. Three branches: nothing emitted (most
// common), a skill emitted, a workflow emitted, or both. The
// skill / workflow paths are independent — both can fire on the
// same session when the LLM judges both artefacts grounded.
func renderInductionResult(out io.Writer, sessionID string, r *prompts.InductionResult) {
	short := preview.ShortID(sessionID)
	if r.Skill == nil && r.Workflow == nil {
		_, _ = fmt.Fprintf(out, "induction: ✓ %s — nothing reusable\n", short)
		if r.Rationale != "" {
			_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
		}
		return
	}
	if r.Skill != nil {
		sk := r.Skill
		_, _ = fmt.Fprintf(out, "induction: ✓ %s — skill %q\n", short, sk.Name)
		if sk.WhenToUse != "" {
			_, _ = fmt.Fprintf(out, "  when_to_use: %s\n", sk.WhenToUse)
		}
		_, _ = fmt.Fprintf(out, "  add: aichronicles propose add --skill %s --output-id <id>\n", sk.Name)
	}
	if r.Workflow != nil {
		wf := r.Workflow
		_, _ = fmt.Fprintf(out, "induction: ✓ %s — workflow %q\n", short, wf.TaskShape)
		for i, step := range wf.Procedure {
			_, _ = fmt.Fprintf(out, "    %d. %s\n", i+1, step.Action)
		}
	}
	if r.Rationale != "" {
		_, _ = fmt.Fprintf(out, "  rationale: %s\n", r.Rationale)
	}
}

// renderInductionList renders the induction history — one row per
// induction llm_outputs row, newest first. Topic is the proposed
// skill name OR "(no skill)" when the verdict was negative.
func renderInductionList(out io.Writer, rows []wire.LLMOutput, format OutputFormat) error {
	if format == FormatJSON {
		// Round-trip the raw rows so a JSON consumer gets the full
		// body and can branch on the skill/workflow fields themselves.
		return emitJSON(out, rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "no induction runs recorded yet — try `aichronicles induction sweep`\n")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s  %-19s  %s\n", "session", "when", "outcome")
	fmt.Fprintln(&b, strings.Repeat("-", 80))
	for _, r := range rows {
		var result prompts.InductionResult
		outcome := "(unparseable body)"
		if jerr := json.Unmarshal([]byte(r.Body), &result); jerr == nil {
			outcome = formatInductionOutcome(&result)
		}
		sessShort := "(none)"
		if r.SessionID != nil && *r.SessionID != "" {
			sessShort = preview.ShortID(*r.SessionID)
		}
		when := timefmt.Absolute(r.CreatedAtMs)
		fmt.Fprintf(&b, "%-8s  %-19s  %s\n", sessShort, when, outcome)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// formatInductionOutcome renders one cell for the induction-list
// table. Three independent shapes (skill / workflow / both / neither)
// each get their own short string.
func formatInductionOutcome(r *prompts.InductionResult) string {
	if r.Skill == nil && r.Workflow == nil {
		return "nothing — " + r.Rationale
	}
	parts := []string{}
	if r.Skill != nil {
		s := "skill: " + r.Skill.Name
		if r.Skill.WhenToUse != "" {
			s += " — " + truncateForList(r.Skill.WhenToUse, 50)
		}
		parts = append(parts, s)
	}
	if r.Workflow != nil {
		w := "workflow: " + truncateForList(r.Workflow.TaskShape, 50)
		parts = append(parts, w)
	}
	return strings.Join(parts, "; ")
}

// truncateForList trims `s` to fit a list cell. Suffix with `…`
// when truncated so the reader knows there's more.
func truncateForList(s string, max int) string {
	// Rune-aware: this trims model-authored prose (when_to_use,
	// task_shape, workflow_change) that routinely contains →, … and
	// ≥, and a byte slice through a multi-byte rune emits U+FFFD.
	// The byte-length guard was also wrong in the other direction —
	// it truncated well before `max` display columns for any
	// non-ASCII text.
	return textfmt.OneLineN(s, max)
}
