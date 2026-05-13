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
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultProposeWindow defaults to a rolling 7 days: long enough
// that a Monday morning "what should I tackle next" still pulls
// in last week's loose threads, short enough that the prompt
// doesn't drown in stale stuff that's already been resolved.
// Override with --since for a tighter or wider net (e.g.
// --since 24h for "today only").
//
// defaultProposeLimit caps how many sessions feed the prompt
// before we send it to the model — same balance as reflect:
// enough sessions to expose patterns, few enough to fit a
// reasonable prompt budget.
const (
	defaultProposeWindow = 7 * 24 * time.Hour
	defaultProposeLimit  = 25
)

func newProposeCmd() *cobra.Command {
	var (
		since     time.Duration
		limit     int
		model     string
		force     bool
		dbPath    string
		sockFlag  string
		formatIn  string
		challenge bool
	)
	cmd := &cobra.Command{
		Use:   "propose",
		Short: "LLM-suggested skills / CLAUDE.md entries / scripts from recent sessions",
		Long: "Reads recent sessions (same window semantics as `reflect`) and,\n" +
			"via the record_proposal tool, asks the LLM to propose concrete\n" +
			"reusable capabilities: new slash-commands, CLAUDE.md rules, and\n" +
			"scripts to pre-build. The system prompt forbids generic advice —\n" +
			"every suggestion must cite at least one session as evidence.\n\n" +
			"Cached on prompt_hash in llm_outputs with kind=propose. Use\n" +
			"--force to re-call. Use --format=json to emit the raw JSON body.\n\n" +
			"Pass --challenge to swap the prompt for forward-looking next-\n" +
			"problem mode (Voyager-style automatic curriculum). The same\n" +
			"digests are fed to a different system prompt that asks 'what\n" +
			"should the user tackle NEXT given their current state?' rather\n" +
			"than 'what patterns recur?'. Output is cached under\n" +
			"kind=challenge so it doesn't collide with the propose cache.\n\n" +
			"Requires " + llm.APIKeyEnv + " unless the cache hits.",
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

			// Header + progress lines go to stderr so a JSON
			// stdout stays clean for piping. Skip progress in
			// JSON mode entirely (RunPropose also silences via
			// the JSON flag, but the header is the cobra layer's
			// concern).
			progress := cmd.ErrOrStderr()
			if format != FormatJSON {
				_, _ = fmt.Fprintf(progress,
					"propose: window=%s  limit=%d  model=%s  provider=%s\n",
					humanDuration(since), limit,
					resolveModelLabel(llmCfg, model),
					providerLabel(llmCfg))
			}

			_, err = RunPropose(ctx, s, c,
				func() (llm.Client, error) {
					return llm.FromConfig(ctx, llmCfg)
				},
				ProposeOptions{
					Since: since, Limit: limit, Model: model,
					Force: force, JSON: format == FormatJSON,
					Challenge: challenge,
					Progress:  progress,
				},
				cmd.OutOrStdout())
			return err
		},
	}
	addFlexDurationFlag(cmd, &since, "since", defaultProposeWindow, "only consider sessions whose ended_at is within this window (e.g. 24h, 7d)")
	cmd.Flags().IntVar(&limit, "limit", defaultProposeLimit, "max sessions to feed the LLM, newest first")
	cmd.Flags().StringVar(&model, "model", "", "LLM model id (default: provider's default)")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the llm_outputs cache and re-call the LLM")
	cmd.Flags().BoolVar(&challenge, "challenge", false,
		"forward-looking mode: propose what to tackle NEXT (Voyager-style curriculum)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	addSocketFlag(cmd, &sockFlag)
	addFormatFlag(cmd, &formatIn)
	cmd.AddCommand(newProposeAddCmd())
	cmd.AddCommand(newProposeMergeCmd())
	cmd.AddCommand(newProposeDiscardCmd())
	cmd.AddCommand(newProposeListCmd())
	return cmd
}

// ProposeOptions drives RunPropose. Shape mirrors ReflectOptions;
// keeping them separate types rather than sharing one avoids
// surprising coupling if one feature grows a flag the other doesn't
// want.
type ProposeOptions struct {
	Since time.Duration
	Limit int
	Model string
	Force bool
	JSON  bool
	// Challenge, when true, swaps the propose prompt for the
	// forward-looking BuildChallenge prompt and persists the
	// result with kind=challenge. Same digest list, different
	// question — see prompts.BuildChallenge for the contract.
	Challenge bool
	// Progress, when non-nil, receives one-line status updates as
	// RunPropose walks its phases (load sessions, enrich, call
	// LLM). Pass io.Discard or leave nil to silence — JSON mode
	// also silences automatically so pipelines see clean output.
	Progress io.Writer
}

// RunPropose orchestrates the proposal path. Same cache + lazy-client
// + clean-on-failure discipline as RunReflect (via runCachedLLM).
//
// Progress lines are written to opts.Progress (when non-nil and not
// in JSON mode) so the user sees what's happening before the
// LLM round-trip lands. Pass io.Discard or leave nil to silence.
func RunPropose(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts ProposeOptions,
	out io.Writer,
) (int64, error) {
	window := opts.Since
	if window <= 0 {
		window = defaultProposeWindow
	}

	progress := opts.Progress
	if progress == nil || opts.JSON {
		progress = io.Discard
	}

	_, _ = fmt.Fprintf(progress, "loading sessions in window=%s (limit=%d)...\n",
		humanDuration(window), opts.Limit)

	sinceMs := time.Now().Add(-window).UnixMilli()
	resp, err := c.SessionDigests(ctx, sinceMs, opts.Limit)
	if err != nil {
		return 0, fmt.Errorf("propose: load sessions: %w", err)
	}
	if len(resp.Digests) == 0 {
		return 0, errors.New("propose: no sessions in the requested window")
	}

	_, _ = fmt.Fprintf(progress, "  loaded %d session(s), enriching with extractions...\n", len(resp.Digests))
	digests, err := digestsFromRowsWithLinks(ctx, c, resp.Digests)
	if err != nil {
		return 0, fmt.Errorf("propose: enrich digests: %w", err)
	}

	// Skill-aware enrichment: list every SKILL.md the user has on
	// disk (global + project-local for each session's start cwd) so
	// the LLM doesn't repropose what's already installed, plus the
	// per-skill invocation counts so it knows which ones the user
	// actively uses. Discovery errors are non-fatal — propose runs
	// without the enrichment rather than refusing to proceed.
	installed, err := skills.CollectInstalled(ctx, s.DB(), sinceMs)
	if err != nil {
		slog.Warn("propose: skipping installed-skills enrichment", "err", err)
	}
	invoked, err := skills.LoadInvoked(ctx, s.DB(), sinceMs)
	if err != nil {
		slog.Warn("propose: skipping invoked-skills enrichment", "err", err)
	}
	// Skill-impact enrichment: per-skill success rate over the
	// same window so the model can see which invoked skills are
	// actually working vs. correlated with tool_failure. Failures
	// here are non-fatal — propose proceeds with bare counts.
	if impactResp, ierr := c.SkillImpact(ctx, wire.SkillImpactRequest{SinceMs: sinceMs}); ierr != nil {
		slog.Warn("propose: skipping skill-impact enrichment", "err", ierr)
	} else {
		invoked = mergeImpactIntoInvoked(invoked, impactResp.Skills)
	}
	_, _ = fmt.Fprintf(progress, "  skills enrichment: %d installed, %d invoked\n",
		len(installed), len(invoked))

	if opts.Challenge {
		return runChallenge(ctx, c, newClient, opts, digests, installed, invoked, out, progress)
	}

	priorProposals, perr := loadPriorProposalsForPrompt(ctx, c, sinceMs)
	if perr != nil {
		slog.Warn("propose: skipping prior-proposals enrichment", "err", perr)
	}
	_, _ = fmt.Fprintf(progress, "  prior-proposals enrichment: %d entries\n", len(priorProposals))

	failureShapes, ferr := loadFailureShapesForPrompt(ctx, c, sinceMs)
	if ferr != nil {
		slog.Warn("propose: skipping failure-shapes enrichment", "err", ferr)
	}
	_, _ = fmt.Fprintf(progress, "  failure-shapes enrichment: %d entries\n", len(failureShapes))

	built, err := prompts.BuildPropose(prompts.ProposeInputs{
		Digests:         digests,
		InstalledSkills: installed,
		InvokedSkills:   invoked,
		PriorProposals:  priorProposals,
		FailureShapes:   failureShapes,
	})
	if err == nil && len(built.Patterns) > 0 {
		slog.Info("propose: egress redaction fired",
			"patterns", strings.Join(built.Patterns, ","))
	}
	if err != nil {
		return 0, fmt.Errorf("propose: build prompt: %w", err)
	}

	_, _ = fmt.Fprintf(progress, "calling LLM (this is the long part — typically 10–30s)...\n")
	result := new(prompts.ProposalResult)
	id, err := runCachedLLM(ctx, c, newClient, cachedLLMInput{
		kind:     store.LLMKindPropose,
		toolName: prompts.ToolNameProposal,
		result:   result,
		hash:     built.Hash,
		req:      built.Request,
		model:    opts.Model,
		force:    opts.Force,
		jsonRaw:  opts.JSON,
		output:   out,
	})
	if err != nil {
		return id, err
	}
	// The LLM output was just persisted via runCachedLLM (cache
	// hit OR cache miss); time.Now is a tight approximation of the
	// row's created_at_ms — close enough for the proposed_at_ms
	// anchor on each candidate. Fetching the exact row back through
	// the api would be one extra round-trip per propose, for sub-
	// second drift on a timestamp the lifecycle treats as advisory.
	recordSkillCandidatesFromProposal(ctx, c, id, time.Now().UnixMilli(), result)
	return id, nil
}

// loadPriorProposalsForPrompt assembles the PriorProposals slice
// rendered in the propose prompt. Joins
// LoadSkillCandidateEffectiveness (every added candidate's post-add
// usage) with LoadPendingSkillCandidates (candidates the user did
// not act on) and dedupes by skill_name (newest entry wins). The
// historical horizon is intentionally LONGER than the propose
// digest window — a skill added 90 days ago is still load-bearing
// context the LLM should see, even if its loads are outside the
// 7-day digest window.
//
// Cap at 30 entries to keep the prompt tokens bounded; the entries
// are most-recently-active first via the underlying queries.
//
// Returns (nil, nil) when both sources are empty — RunPropose
// silently proceeds without the stanza (renderPriorProposals
// returns empty for empty input).
func loadPriorProposalsForPrompt(ctx context.Context, c *apiclient.Client, sinceMs int64) ([]prompts.PriorProposal, error) {
	// Look back further than the digest window for prior proposals:
	// a 90-day horizon catches "you proposed this 60 days ago and
	// it never got applied / never got loaded" — load-bearing
	// signal even outside the 7d propose window.
	const priorHorizonDays = 90
	priorSinceMs := time.Now().Add(-priorHorizonDays * 24 * time.Hour).UnixMilli()
	_ = sinceMs // intentionally not used; the propose digest window
	// is too narrow for the lifecycle stanza.

	const maxEntries = 30

	addedResp, err := c.SkillCandidatesEffectiveness(ctx, wire.SkillCandidateEffectivenessRequest{
		SinceMs: priorSinceMs,
		Limit:   maxEntries,
	})
	if err != nil {
		return nil, fmt.Errorf("load skill candidate effectiveness: %w", err)
	}
	pendingResp, err := c.SkillCandidatesPending(ctx, priorSinceMs, maxEntries)
	if err != nil {
		return nil, fmt.Errorf("load pending skill candidates: %w", err)
	}

	// Build one map keyed by skill_name; on a clash, the added
	// row wins (the lifecycle moved forward, the pending entry
	// is now stale).
	out := make([]prompts.PriorProposal, 0, len(addedResp.Rows)+len(pendingResp.Candidates))
	seen := make(map[string]struct{}, len(addedResp.Rows)+len(pendingResp.Candidates))
	for _, e := range addedResp.Rows {
		if _, dup := seen[e.SkillName]; dup {
			continue
		}
		seen[e.SkillName] = struct{}{}
		var lastLoaded int64
		if e.LastLoadedMs != nil {
			lastLoaded = *e.LastLoadedMs
		}
		out = append(out, prompts.PriorProposal{
			SkillName:        e.SkillName,
			ProposedAtMs:     e.ProposedAtMs,
			Added:            true,
			AddedAtMs:        e.AddedAtMs,
			LoadsAfterAdd:    e.LoadsAfterAdd,
			FailedLoadsAfter: e.FailedLoadsAfter,
			LastLoadedMs:     lastLoaded,
		})
	}
	for _, u := range pendingResp.Candidates {
		if _, dup := seen[u.SkillName]; dup {
			continue
		}
		seen[u.SkillName] = struct{}{}
		out = append(out, prompts.PriorProposal{
			SkillName:    u.SkillName,
			ProposedAtMs: u.ProposedAtMs,
			Added:        false,
		})
		if len(out) >= maxEntries {
			break
		}
	}
	return out, nil
}

// loadFailureShapesForPrompt assembles the FailureShapes slice
// rendered as a contrastive corpus in the propose prompt. Pulls
// session_outcomes rows where outcome=failure_likely from the same
// window the digests use, capped so the stanza stays compact.
//
// The stanza is the negative half of ExpeL-style insight extraction:
// the LLM also considers skills that PREVENT recurring failure modes
// (system rule 13). Without it, propose only mines successful
// patterns and misses the friction-prevention class entirely.
//
// Best-effort: a load error returns empty + nil so RunPropose
// proceeds without the stanza rather than failing the LLM call.
func loadFailureShapesForPrompt(ctx context.Context, c *apiclient.Client, sinceMs int64) ([]prompts.FailureShapeDigest, error) {
	resp, err := c.FailureShapes(ctx, sinceMs, 0)
	if err != nil {
		return nil, fmt.Errorf("load failure shapes: %w", err)
	}
	out := make([]prompts.FailureShapeDigest, 0, len(resp.Shapes))
	for _, r := range resp.Shapes {
		fs := prompts.FailureShapeDigest{
			SessionID:         r.SessionID,
			Title:             r.Title,
			ToolFailureCount:  r.ToolFailureCount,
			GitUndoCount:      r.GitUndoCount,
			PromptRepeatCount: r.PromptRepeatCount,
		}
		if r.Cwd != nil {
			fs.Cwd = *r.Cwd
		}
		if r.LastEventKind != nil {
			fs.LastEventKind = *r.LastEventKind
		}
		out = append(out, fs)
	}
	return out, nil
}

// recordSkillCandidatesFromProposal writes one skill_candidates row
// per skill in the proposal, capturing the AutoSkill 7-tuple
// metadata (triggers, tags, examples, version) the LLM emitted.
// Best-effort: lifecycle tracking failures are logged but do not
// propagate, so a transient DB error here doesn't make the user
// think their LLM call failed when in fact the cached row landed
// cleanly.
//
// proposed_at_ms anchors to the llm_outputs row's created_at_ms so
// re-runs that hit the cache write the same proposed_at_ms — the
// PK is (llm_output_id, skill_name) so the upsert in
// RecordSkillCandidateWithMetadata keeps writes idempotent
// regardless.
func recordSkillCandidatesFromProposal(ctx context.Context, c *apiclient.Client, llmOutputID int64, createdAtMs int64, r *prompts.ProposalResult) {
	if r == nil || llmOutputID <= 0 {
		return
	}
	if createdAtMs <= 0 {
		// Without a proposed_at_ms anchor the api will reject the
		// record. Fall back to "now" rather than dropping the
		// candidate entirely — the lifecycle row stays usable, the
		// timestamp is just slightly less precise than the LLM
		// row's created_at.
		createdAtMs = time.Now().UnixMilli()
	}
	for _, sk := range r.Skills {
		if sk.Name == "" {
			continue
		}
		req := wire.RecordSkillCandidateRequest{
			LLMOutputID:  llmOutputID,
			SkillName:    sk.Name,
			ProposedAtMs: createdAtMs,
			Metadata:     skillMetadataFromProposed(sk),
		}
		if _, rerr := c.RecordSkillCandidate(ctx, req); rerr != nil {
			slog.Warn("propose: failed to record skill candidate",
				"llm_output_id", llmOutputID, "skill", sk.Name, "err", rerr)
		}
	}
}

// skillMetadataFromProposed lifts the AutoSkill 7-tuple metadata
// (triggers τ, tags γ, examples ξ, version v) plus the contrastive
// kind label from a prompts.ProposedSkill into the wire-shape
// wire.SkillCandidateMetadata. Centralised so the propose and
// induction call paths can't drift on the field mapping.
func skillMetadataFromProposed(sk prompts.ProposedSkill) wire.SkillCandidateMetadata {
	examples := make([]wire.SkillCandidateExample, 0, len(sk.Examples))
	for _, e := range sk.Examples {
		examples = append(examples, wire.SkillCandidateExample{
			Input:  e.Input,
			Output: e.Output,
		})
	}
	kind := sk.Kind
	if kind != string(store.SkillKindPattern) && kind != string(store.SkillKindPitfall) {
		// LLM omitted the field or emitted something out-of-enum;
		// the api defaults to "pattern" anyway, so the explicit
		// fallback here keeps the mapping reversible.
		kind = ""
	}
	return wire.SkillCandidateMetadata{
		Triggers: append([]string(nil), sk.Triggers...),
		Tags:     append([]string(nil), sk.Tags...),
		Examples: examples,
		Kind:     kind,
		// Version is set by the store at insert time
		// (InitialSkillVersion) when meta.Version is empty —
		// new candidates always start at v0.1.0; the merge path
		// is what bumps the patch.
	}
}

// runChallenge is the --challenge path. Branched from RunPropose
// rather than inlined so the prompt-shape, persistence kind, and
// open-threads enrichment stay together — RunPropose's main path
// would otherwise grow a tangled if/else over every phase.
//
// Open-threads enrichment: pulls unresolved items from prior
// sessions across the digest's cwds. Same source as
// `aichronicles unresolved` and the get_unresolved_for_cwd MCP
// tool — keeps the three surfaces consistent on what counts as
// an "open thread".
func runChallenge(
	ctx context.Context,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts ProposeOptions,
	digests []prompts.SessionDigest,
	installed []prompts.InstalledSkill,
	invoked []prompts.InvokedSkill,
	out, progress io.Writer,
) (int64, error) {
	// Pull unresolved items for each distinct cwd in the digests.
	// A small set in practice (the user usually works in 1-3
	// projects per window); cap at 30 items total to keep the
	// prompt compact.
	seenCwd := make(map[string]struct{})
	var open []prompts.UnresolvedItemForChallenge
	const maxItems = 30
	sinceMs := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	for _, d := range digests {
		if d.Cwd == "" {
			continue
		}
		if _, dup := seenCwd[d.Cwd]; dup {
			continue
		}
		seenCwd[d.Cwd] = struct{}{}
		uresp, err := c.Unresolved(ctx, apiclient.UnresolvedRequest{
			Cwd:                d.Cwd,
			SinceMs:            sinceMs,
			MaxSessions:        5,
			MaxItemsPerSession: 5,
		})
		if err != nil {
			slog.Warn("challenge: skipping unresolved enrichment for cwd",
				"cwd", d.Cwd, "err", err)
			continue
		}
		for _, it := range uresp.Items {
			if len(open) >= maxItems {
				break
			}
			open = append(open, prompts.UnresolvedItemForChallenge{
				SessionID:    it.SessionID,
				SessionShort: it.SessionShort,
				Topic:        it.Topic,
				Item:         it.Item,
			})
		}
		if len(open) >= maxItems {
			break
		}
	}
	_, _ = fmt.Fprintf(progress, "  open-threads enrichment: %d items across %d cwd(s)\n",
		len(open), len(seenCwd))

	built, err := prompts.BuildChallenge(prompts.ChallengeInputs{
		Digests:         digests,
		InstalledSkills: installed,
		InvokedSkills:   invoked,
		Unresolved:      open,
	})
	if err != nil {
		return 0, fmt.Errorf("challenge: build prompt: %w", err)
	}
	if len(built.Patterns) > 0 {
		slog.Info("challenge: egress redaction fired",
			"patterns", strings.Join(built.Patterns, ","))
	}

	_, _ = fmt.Fprintf(progress, "calling LLM (challenge mode)...\n")
	return runCachedLLM(ctx, c, newClient, cachedLLMInput{
		kind:     store.LLMKindChallenge,
		toolName: prompts.ToolNameChallenge,
		result:   new(prompts.ChallengeResult),
		hash:     built.Hash,
		req:      built.Request,
		model:    opts.Model,
		force:    opts.Force,
		jsonRaw:  opts.JSON,
		output:   out,
	})
}

// mergeImpactIntoInvoked enriches each InvokedSkill in `invoked`
// with success-rate fields from a same-window LoadSkillImpact
// scan. Skills that have an impact row get their TotalLoads /
// FailedLoads / SuccessRate populated; skills without one stay
// untouched (the prompt template falls back to the bare-count
// rendering for those).
//
// Two queries instead of a single JOIN because impact comes from
// store and invoked from internal/skills — keeping them
// independent means LoadInvoked stays small and reusable, and
// adding the impact source is a 5-line splice in propose.go
// rather than a new shape parameter on every caller of LoadInvoked.
func mergeImpactIntoInvoked(invoked []prompts.InvokedSkill, impact []wire.SkillImpact) []prompts.InvokedSkill {
	if len(impact) == 0 {
		return invoked
	}
	byName := make(map[string]wire.SkillImpact, len(impact))
	for _, im := range impact {
		byName[im.Name] = im
	}
	for i := range invoked {
		if im, ok := byName[invoked[i].Name]; ok {
			invoked[i].TotalLoads = im.TotalLoads
			invoked[i].FailedLoads = im.FailedLoads
			invoked[i].SuccessRate = im.SuccessRate
			invoked[i].LastLoadedMs = im.LastLoadedMs
		}
	}
	return invoked
}
