package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// MetaAnalysisKinds enumerates the cadence-gated meta-analyses
// the meta sweep drives. The set is closed: adding a kind here is
// a deliberate choice (per-kind cadence, per-kind skip flag, per-
// kind dispatch) and unknown kinds in config are rejected so a
// typo can't silently disable a feature.
const (
	MetaKindPropose       = "propose"
	MetaKindReflect       = "reflect"
	MetaKindChallenge     = "challenge"
	MetaKindReflectWeekly = "reflect_weekly"
	MetaKindSkillRevision = "skill_revision"
)

// Cadence defaults for the meta sweep. Applied by
// MetaAnalysisSweepOptionsFromConfig when the operator left a
// field zero. Match the prompts' natural horizons (running them
// more often produces near-identical outputs at full LLM cost).
const (
	DefaultMetaProposeCadence           = 24 * time.Hour
	DefaultMetaReflectCadence           = 7 * 24 * time.Hour
	DefaultMetaChallengeCadence         = 7 * 24 * time.Hour
	DefaultMetaReflectWeeklyCadence     = 7 * 24 * time.Hour
	DefaultMetaSkillRevisionCadence     = 24 * time.Hour
	DefaultMetaSkillRevisionMinRate     = 0.5
	DefaultMetaSkillRevisionMaxPerSweep = 5
	DefaultMetaSkillRevisionSince       = 30 * 24 * time.Hour
)

// MetaAnalysisSweepOptionsFromConfig converts the toml-shaped
// config block into typed sweep options, applying built-in
// defaults where the operator left a field zero. Shared between
// the `aichronicles meta sweep` subcommand and any other caller
// that drives RunMetaAnalysisSweep, so defaulting rules can't
// drift between paths.
func MetaAnalysisSweepOptionsFromConfig(cfg config.MetaAnalysis) MetaAnalysisSweepOptions {
	opts := MetaAnalysisSweepOptions{
		ProposeCadence:       cfg.ProposeCadence.Or(DefaultMetaProposeCadence),
		ProposeSkip:          cfg.ProposeSkip,
		ProposeSinceWindow:   cfg.ProposeSinceWindow.Or(0),
		ProposeLimit:         cfg.ProposeLimit,
		ReflectCadence:       cfg.ReflectCadence.Or(DefaultMetaReflectCadence),
		ReflectSkip:          cfg.ReflectSkip,
		ReflectSinceWindow:   cfg.ReflectSinceWindow.Or(0),
		ReflectLimit:         cfg.ReflectLimit,
		ChallengeCadence:     cfg.ChallengeCadence.Or(DefaultMetaChallengeCadence),
		ChallengeSkip:        cfg.ChallengeSkip,
		ChallengeSinceWindow: cfg.ChallengeSinceWindow.Or(0),
		ChallengeLimit:       cfg.ChallengeLimit,
		ReflectWeeklyCadence: cfg.ReflectWeeklyCadence.Or(DefaultMetaReflectWeeklyCadence),
		ReflectWeeklySkip:    cfg.ReflectWeeklySkip,
		SkillRevisionCadence: cfg.SkillRevisionCadence.Or(DefaultMetaSkillRevisionCadence),
		SkillRevisionSkip:    cfg.SkillRevisionSkip,
		SkillRevisionSince:   cfg.SkillRevisionSince.Or(DefaultMetaSkillRevisionSince),
		SkillRevisionWindow:  cfg.SkillRevisionWindow.Or(0),
		SkillRevisionMinRate: cfg.SkillRevisionMinRate,
		SkillRevisionMax:     cfg.SkillRevisionMax,
		Model:                cfg.Model,
	}
	if opts.SkillRevisionMinRate <= 0 {
		opts.SkillRevisionMinRate = DefaultMetaSkillRevisionMinRate
	}
	if opts.SkillRevisionMax <= 0 {
		opts.SkillRevisionMax = DefaultMetaSkillRevisionMaxPerSweep
	}
	return opts
}

// MetaAnalysisSweepOptions drives RunMetaAnalysisSweep. Each per-
// kind block carries its own cadence and skip flag; per-kind
// dispatch parameters (limits, windows) live as fields too rather
// than as opaque maps so the call sites stay typed.
//
// Cadences are ABSOLUTE: a kind fires when
// `now - max(created_at_ms WHERE kind=X) >= Cadence[X]`. First-run
// case is `now - 0 >= cadence` → fires immediately. The cadence
// gate runs against the persisted timestamp, not an in-memory
// last-fired marker, so a daemon restart does not double-fire.
type MetaAnalysisSweepOptions struct {
	// ProposeCadence / ProposeSkip control how often the propose
	// path fires. ProposeSinceWindow / ProposeLimit feed
	// ProposeOptions; defaults match the CLI command when zero.
	ProposeCadence     time.Duration
	ProposeSkip        bool
	ProposeSinceWindow time.Duration
	ProposeLimit       int

	// ReflectCadence / ReflectSkip control the reflect path.
	// ReflectSinceWindow / ReflectLimit mirror ReflectOptions.
	ReflectCadence     time.Duration
	ReflectSkip        bool
	ReflectSinceWindow time.Duration
	ReflectLimit       int

	// ChallengeCadence / ChallengeSkip control the forward-looking
	// (Voyager-style curriculum) variant of propose.
	ChallengeCadence     time.Duration
	ChallengeSkip        bool
	ChallengeSinceWindow time.Duration
	ChallengeLimit       int

	// ReflectWeeklyCadence / ReflectWeeklySkip control the weekly
	// digest. The period is anchored to the previous completed
	// Monday-00:00-UTC week (same as the CLI command).
	ReflectWeeklyCadence time.Duration
	ReflectWeeklySkip    bool

	// SkillRevisionCadence / SkillRevisionSkip control how often
	// the per-stale-skill evolve pass runs. SkillsDir overrides
	// the discovery root (empty → ~/.claude/skills via
	// resolveSkillsDir). SkillRevisionMinRate filters which
	// stale-correlated skills are eligible — a skill must have its
	// Wilson-score 95%-CI lower bound on failed-load rate ≥ this
	// fraction (in [0,1]) to be revised automatically. The lower
	// bound (rather than the naive rate) keeps low-N noise out: a
	// 1/1 stale skill has rate=1.0 but lower bound ~0.21, while a
	// 50/100 skill has rate=0.5 with bound ~0.40. Defaults to 0.5
	// when zero.
	SkillRevisionCadence time.Duration
	SkillRevisionSkip    bool
	SkillsDir            string
	SkillRevisionMinRate float64
	SkillRevisionSince   time.Duration
	SkillRevisionWindow  time.Duration
	SkillRevisionMax     int

	// Model, when non-empty, overrides the LLM model id for every
	// call this sweep makes. Empty falls back to the provider's
	// default.
	Model string
}

// RunMetaAnalysisSweep is the orchestrator the daemon's
// MetaAnalysisSweeper wraps. For each meta kind, it checks the
// cadence (against the most-recent persisted row), and if the kind
// is overdue, dispatches to the appropriate Run* function.
//
// Per-kind failure isolation: a failure in one kind logs and
// continues to the next. The sweep returns the first non-nil
// error encountered, but ALL eligible kinds are attempted before
// returning — so a transient propose failure does not skip the
// week's reflect digest.
//
// context.Canceled errors are folded into the return path
// untouched (so the daemon's tick wrapper recognises shutdown).
func RunMetaAnalysisSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts MetaAnalysisSweepOptions,
	out, errOut io.Writer,
) error {
	if s == nil {
		return errors.New("meta sweep: nil store")
	}
	if newClient == nil {
		return errors.New("meta sweep: nil newClient")
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}

	var firstErr error
	record := func(err error) {
		if err == nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	now := time.Now()

	// propose
	if !opts.ProposeSkip {
		if due, err := overdue(ctx, c, store.LLMKindPropose, opts.ProposeCadence, now); err != nil {
			record(err)
		} else if due {
			if err := runProposeForSweep(ctx, s, c, newClient, opts, out, errOut); err != nil {
				slog.Warn("meta sweep: propose failed", "err", err)
				record(err)
			}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
	}

	// reflect
	if !opts.ReflectSkip {
		if due, err := overdue(ctx, c, store.LLMKindReflect, opts.ReflectCadence, now); err != nil {
			record(err)
		} else if due {
			if err := runReflectForSweep(ctx, s, c, newClient, opts, out, errOut); err != nil {
				slog.Warn("meta sweep: reflect failed", "err", err)
				record(err)
			}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
	}

	// challenge
	if !opts.ChallengeSkip {
		if due, err := overdue(ctx, c, store.LLMKindChallenge, opts.ChallengeCadence, now); err != nil {
			record(err)
		} else if due {
			if err := runChallengeForSweep(ctx, s, c, newClient, opts, out, errOut); err != nil {
				slog.Warn("meta sweep: challenge failed", "err", err)
				record(err)
			}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
	}

	// reflect_weekly
	if !opts.ReflectWeeklySkip {
		if due, err := overdue(ctx, c, store.LLMKindReflectWeekly, opts.ReflectWeeklyCadence, now); err != nil {
			record(err)
		} else if due {
			if err := runReflectWeeklyForSweep(ctx, s, c, newClient, opts, out, errOut, now); err != nil {
				slog.Warn("meta sweep: reflect_weekly failed", "err", err)
				record(err)
			}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
	}

	// skill_revision
	if !opts.SkillRevisionSkip {
		if due, err := overdue(ctx, c, store.LLMKindSkillRevision, opts.SkillRevisionCadence, now); err != nil {
			record(err)
		} else if due {
			if err := runSkillRevisionForSweep(ctx, s, c, newClient, opts, out, errOut); err != nil {
				slog.Warn("meta sweep: skill_revision failed", "err", err)
				record(err)
			}
		}
	}

	return firstErr
}

// overdue returns true when at least `cadence` has elapsed since
// the most recent llm_outputs row of the given kind. cadence <=
// 0 means "never fire" — the kind is effectively disabled even
// without setting Skip (so an operator who unsets a cadence
// without unsetting Skip still gets safe behaviour).
func overdue(
	ctx context.Context,
	c *apiclient.Client,
	kind store.LLMOutputKind,
	cadence time.Duration,
	now time.Time,
) (bool, error) {
	if cadence <= 0 {
		return false, nil
	}
	last, err := c.LLMOutputLastCreatedAt(ctx, string(kind))
	if err != nil {
		return false, fmt.Errorf("read last %s timestamp: %w", kind, err)
	}
	// last == 0 → never fired → fire now.
	elapsed := now.Sub(time.UnixMilli(last))
	return elapsed >= cadence, nil
}

func runProposeForSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts MetaAnalysisSweepOptions,
	out, errOut io.Writer,
) error {
	since := opts.ProposeSinceWindow
	if since <= 0 {
		since = defaultProposeWindow
	}
	limit := opts.ProposeLimit
	if limit <= 0 {
		limit = defaultProposeLimit
	}
	_, _ = fmt.Fprintln(errOut, "meta sweep: dispatching propose")
	_, err := RunPropose(ctx, s, c, newClient, ProposeOptions{
		Since:    since,
		Limit:    limit,
		Model:    opts.Model,
		Progress: errOut,
	}, out)
	// "no sessions in window" is not a sweeper failure — the
	// system is just quiet. Surface as info, not error.
	if err != nil && isEmptyWindowErr(err) {
		_, _ = fmt.Fprintf(errOut, "  propose: %v (skipping)\n", err)
		return nil
	}
	return err
}

func runReflectForSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts MetaAnalysisSweepOptions,
	out, errOut io.Writer,
) error {
	since := opts.ReflectSinceWindow
	if since <= 0 {
		since = defaultReflectWindow
	}
	limit := opts.ReflectLimit
	if limit <= 0 {
		limit = defaultReflectLimit
	}
	_, _ = fmt.Fprintln(errOut, "meta sweep: dispatching reflect")
	_, err := RunReflect(ctx, s, c, newClient, ReflectOptions{
		Since: since,
		Limit: limit,
		Model: opts.Model,
	}, out)
	if err != nil && isEmptyWindowErr(err) {
		_, _ = fmt.Fprintf(errOut, "  reflect: %v (skipping)\n", err)
		return nil
	}
	return err
}

func runChallengeForSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts MetaAnalysisSweepOptions,
	out, errOut io.Writer,
) error {
	since := opts.ChallengeSinceWindow
	if since <= 0 {
		since = defaultProposeWindow
	}
	limit := opts.ChallengeLimit
	if limit <= 0 {
		limit = defaultProposeLimit
	}
	_, _ = fmt.Fprintln(errOut, "meta sweep: dispatching challenge")
	_, err := RunPropose(ctx, s, c, newClient, ProposeOptions{
		Since:     since,
		Limit:     limit,
		Model:     opts.Model,
		Challenge: true,
		Progress:  errOut,
	}, out)
	if err != nil && isEmptyWindowErr(err) {
		_, _ = fmt.Fprintf(errOut, "  challenge: %v (skipping)\n", err)
		return nil
	}
	return err
}

func runReflectWeeklyForSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts MetaAnalysisSweepOptions,
	out, errOut io.Writer,
	now time.Time,
) error {
	start, end, err := resolveWeekBounds("", now.UTC())
	if err != nil {
		return fmt.Errorf("resolve week bounds: %w", err)
	}
	_, _ = fmt.Fprintf(errOut, "meta sweep: dispatching reflect_weekly week=%s\n",
		start.Format("2006-01-02"))
	_, err = RunDigestWeekly(ctx, s, c, newClient, DigestWeeklyOptions{
		PeriodStart: start,
		PeriodEnd:   end,
		Limit:       weeklyDigestSessionLimit,
		Model:       opts.Model,
	}, out)
	if err != nil && isEmptyWindowErr(err) {
		_, _ = fmt.Fprintf(errOut, "  reflect_weekly: %v (skipping)\n", err)
		return nil
	}
	return err
}

// runSkillRevisionForSweep iterates the staleness report and
// dispatches one RunSkillsEvolve per qualifying skill. Cadence is
// gated on `kind=skill_revision` overall (not per-skill), so a
// single tick may produce multiple revisions; the prompt-hash
// cache prevents re-spending on a SKILL whose body hasn't changed
// since the last sweep.
func runSkillRevisionForSweep(
	ctx context.Context,
	s *store.Store,
	c *apiclient.Client,
	newClient func() (llm.Client, error),
	opts MetaAnalysisSweepOptions,
	out, errOut io.Writer,
) error {
	since := opts.SkillRevisionSince
	if since <= 0 {
		since = 30 * 24 * time.Hour
	}
	window := opts.SkillRevisionWindow
	if window <= 0 {
		window = defaultSkillStaleWindow
	}
	maxSkills := opts.SkillRevisionMax
	if maxSkills <= 0 {
		maxSkills = 5
	}
	minRate := opts.SkillRevisionMinRate
	if minRate <= 0 {
		minRate = 0.5
	}

	sinceMs := time.Now().Add(-since).UnixMilli()
	report, err := store.LoadSkillStaleness(ctx, s.DB(), sinceMs, window.Milliseconds(),
		store.SkillStalenessLimits{})
	if err != nil {
		return fmt.Errorf("load staleness report: %w", err)
	}
	if len(report) == 0 {
		_, _ = fmt.Fprintln(errOut, "meta sweep: skill_revision: no stale skills, nothing to do")
		return nil
	}

	skillsDir := resolveSkillsDir(opts.SkillsDir)
	dispatched := 0
	var firstErr error
	for _, st := range report {
		if dispatched >= maxSkills {
			break
		}
		if st.RateLowerBound < minRate {
			continue
		}
		_, _ = fmt.Fprintf(errOut,
			"meta sweep: dispatching skill_revision skill=%s rate=%.0f%% lb=%.0f%%\n",
			st.Name, st.Rate*100, st.RateLowerBound*100)
		dispatched++
		if err := RunSkillsEvolve(ctx, s, c, newClient, SkillsEvolveOptions{
			SkillName: st.Name,
			SkillsDir: skillsDir,
			Since:     since,
			Window:    window,
			Model:     opts.Model,
		}, out, errOut); err != nil {
			slog.Warn("meta sweep: skill revision failed",
				"skill", st.Name, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			continue
		}
	}
	return firstErr
}

// isEmptyWindowErr recognises the "no sessions in window" sentinel
// strings the Run* functions return. We don't want a quiet system
// to look like a meta-sweep failure in the operator's log.
//
// String-matching isn't pretty but the underlying errors are
// constructed via errors.New / fmt.Errorf without sentinel values,
// and sticking sentinel values in just for this gate would
// over-couple the sweep to the orchestrators. The fragments are
// stable and covered by tests.
func isEmptyWindowErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, frag := range []string{
		"no sessions in the requested window",
		"no sessions in week of",
		"no summarised sessions in week of",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
