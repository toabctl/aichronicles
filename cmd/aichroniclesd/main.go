// aichroniclesd is the ingest daemon: a Unix-socket HTTP server that
// accepts envelopes on POST /v1/ingest and persists them to SQLite.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
)

// defaultShutdownDrainTimeout caps how long the daemon will wait for
// in-flight requests to finish after SIGTERM / SIGINT. systemd's
// default TimeoutStopSec is 90s; 10s is comfortably under that while
// still letting a slow SQLite write commit. Operators can override
// via [limits].shutdown_drain_timeout in the config file.
const defaultShutdownDrainTimeout = 10 * time.Second

func main() {
	if err := newRootCmd().Execute(); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("aichroniclesd failed", "err", err)
		os.Exit(1)
	}
}

// newRootCmd builds the cobra command for the daemon. Cobra gets us
// --version (bound to cli.Version, shared with the aichronicles
// binary so both stamps line up) and consistent --help formatting,
// at the cost of dragging cobra's transitive deps in. The runtime
// behaviour — flags, signal handling, drain — is unchanged.
func newRootCmd() *cobra.Command {
	var (
		sockFlag string
		dbFlag   string
	)
	cmd := &cobra.Command{
		Use:           "aichroniclesd",
		Short:         "Ingest daemon for aichronicles",
		Long:          "aichroniclesd is the ingest daemon. It accepts envelopes on POST /v1/ingest over a Unix domain socket and persists them to SQLite. Run via systemd --user with socket activation, or directly for development.",
		Version:       cli.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(sockFlag, dbFlag)
		},
	}
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"unix socket path (overrides $AICHRONICLES_SOCKET; defaults to XDG_RUNTIME_DIR)")
	cmd.Flags().StringVar(&dbFlag, "db", "",
		"SQLite store path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

func run(sockFlag, dbFlag string) error {
	resolvedSock, err := paths.ResolveSocketPath(sockFlag)
	if err != nil {
		return fmt.Errorf("resolve socket path: %w", err)
	}
	resolvedDB, err := paths.ResolveStorePath(dbFlag)
	if err != nil {
		return fmt.Errorf("resolve store path: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Warn("config load failed, using defaults", "err", err)
		d := config.Default()
		cfg = &d
	}

	if err := os.MkdirAll(filepath.Dir(resolvedDB), 0o700); err != nil {
		return fmt.Errorf("ensure store dir: %w", err)
	}
	st, err := store.Open(resolvedDB)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	st.SetMaxOpenConns(cfg.Limits.SQLiteMaxOpenConns)

	srv := daemon.NewServer(st, logger).WithMaxEnvelopeBytes(cfg.Limits.MaxEnvelopeBytes)

	var shutdown func(context.Context) error
	activationListener, err := daemon.ListenFromSystemd()
	if err != nil {
		return fmt.Errorf("systemd socket activation: %w", err)
	}
	var startMsg string
	if activationListener != nil {
		shutdown = daemon.Serve(activationListener, srv.Handler())
		startMsg = "socket-activated by systemd"
		logger.Info("aichroniclesd started (socket-activated by systemd)", "db", resolvedDB)
	} else {
		shutdown, err = daemon.ListenAndServe(resolvedSock, srv.Handler())
		if err != nil {
			return err
		}
		startMsg = "listener at " + resolvedSock
		logger.Info("aichroniclesd started", "socket", resolvedSock, "db", resolvedDB)
	}

	if err := notify.New(cfg.Notifications.DaemonStart).Send("aichronicles started", startMsg); err != nil {
		logger.Warn("start notification failed", "err", err)
	}

	// signal.NotifyContext gives us a cancellable context that fires
	// on SIGTERM/SIGINT. We use it purely to wait — the actual drain
	// deadline lives on a separate bounded child below.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tell systemd we're up. Order matters: this MUST come after
	// the listener is bound (above), otherwise systemd considers
	// us ready while we're still starting and may fire READY-
	// dependent units against a not-yet-accepting daemon. No-op
	// when not under a notify-type service.
	daemon.NotifyReady(logger)

	// Start the watchdog AFTER READY=1 so the first probe goes
	// against an actually-up listener. Bound to sigCtx so a
	// shutdown signal stops the watchdog goroutine cleanly
	// alongside the rest of the daemon. No-op when WATCHDOG_USEC
	// is unset.
	if err := daemon.Start(sigCtx, resolvedSock, logger); err != nil {
		logger.Warn("start watchdog", "err", err)
	}

	// Online induction sweeper — disabled-by-default. When enabled,
	// the goroutine periodically asks the LLM whether each newly-
	// idle session contained a reusable workflow worth saving as a
	// skill. See config.Induction; cli.RunInductionSweep is the
	// callback. No-op when cfg.Induction.Enabled is false.
	if cfg.Induction.Enabled {
		startInductionSweeper(sigCtx, st, cfg, logger)
	}

	// Meta-analysis sweeper — disabled-by-default. When enabled,
	// the goroutine fires propose/reflect/challenge/digest_weekly/
	// skill_revision on per-kind cadences. See config.MetaAnalysis;
	// cli.RunMetaAnalysisSweep is the callback. No-op when
	// cfg.MetaAnalysis.Enabled is false.
	if cfg.MetaAnalysis.Enabled {
		startMetaAnalysisSweeper(sigCtx, st, cfg, logger)
	}

	<-sigCtx.Done()
	drainTimeout := cfg.Limits.ShutdownDrainTimeout.Or(defaultShutdownDrainTimeout)
	logger.Info("aichroniclesd shutting down", "drain_timeout", drainTimeout)

	// Tell systemd we're shutting down deliberately so it can
	// distinguish a clean exit from a watchdog-driven kill in
	// the journal. Issued before drain so it lands even if a
	// pending request takes the full drain window.
	daemon.NotifyStopping(logger)

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return shutdown(drainCtx)
}

// defaultInductionSweepInterval — the daemon-resident sweeper's
// cadence when the user hasn't configured one. 15 minutes is a
// sweet spot: short enough that a finished session gets induced
// while it's still cognitively warm, long enough that the LLM
// spend amortises (with the per-session cache, re-running on the
// same body is free).
const defaultInductionSweepInterval = 15 * time.Minute

// defaultInductionSweepMaxPerTick caps the per-sweep blast
// radius. With ~50¢/induction typical, 5 per tick keeps the
// worst case at ~$10/hour even with a pathological backlog of
// idle sessions. The next tick drains the rest.
const defaultInductionSweepMaxPerTick = 5

// startInductionSweeper spawns the daemon-resident induction
// goroutine. Pulled out of run() to keep the main path focused;
// the actual sweep work happens in cli.RunInductionSweep, which
// the goroutine wraps in the Sweep callback.
//
// LLM client construction is deferred per-tick (`llm.FromConfig`
// is called inside the callback, not closed over here) so a
// transient credentials issue at daemon start doesn't permanently
// disable the sweeper — the next tick retries with fresh config.
func startInductionSweeper(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) {
	interval := cfg.Induction.SweepInterval.Or(defaultInductionSweepInterval)
	idle := cfg.Induction.Idle.Or(30 * time.Minute)
	minEvents := cfg.Induction.MinEvents
	if minEvents <= 0 {
		minEvents = 5
	}
	maxPerSweep := cfg.Induction.MaxPerSweep
	if maxPerSweep <= 0 {
		maxPerSweep = defaultInductionSweepMaxPerTick
	}
	llmCfg := cli.LLMConfigFromFile(cfg.LLM)

	sw := &daemon.InductionSweeper{
		Interval: interval,
		Log:      log,
		Sweep: func(sctx context.Context) error {
			return cli.RunInductionSweep(sctx, st,
				func() (llm.Client, error) {
					return llm.FromConfig(sctx, llmCfg)
				},
				cli.InductionSweepOptions{
					Idle:             idle,
					MinEvents:        minEvents,
					Limit:            maxPerSweep,
					SkipSummarize:    cfg.Induction.SkipSummarize,
					SkipFacts:        cfg.Induction.SkipFacts,
					SkipEpisodes:     cfg.Induction.SkipEpisodes,
					SummarizeTimeout: cfg.Limits.SummarizeTimeout.Or(3 * time.Minute),
					InductionTimeout: cfg.Limits.ReflectTimeout.Or(5 * time.Minute),
					FactsTimeout:     cfg.Limits.SummarizeTimeout.Or(3 * time.Minute),
				},
				daemon.DiscardWriter, // stdout has no audience here
				daemon.DiscardWriter, // stderr telemetry duplicates slog calls inside the sweep
			)
		},
	}
	go sw.Run(ctx)
	log.Info("induction sweeper enabled",
		"interval", interval, "idle", idle,
		"min_events", minEvents, "max_per_sweep", maxPerSweep,
		"skip_summarize", cfg.Induction.SkipSummarize,
		"skip_facts", cfg.Induction.SkipFacts,
		"skip_episodes", cfg.Induction.SkipEpisodes)
}

// defaultMetaAnalysisInterval is the cadence-check tick. 1h is
// fine: per-kind cadences are measured in days, so one wakeup per
// hour is plenty (and a tighter loop would mostly burn cycles
// finding "still in cadence, nothing to do").
const defaultMetaAnalysisInterval = time.Hour

// startMetaAnalysisSweeper spawns the daemon-resident meta-analysis
// goroutine. Pulled out of run() to keep the main path focused;
// the actual cadence-gated dispatch happens in
// cli.RunMetaAnalysisSweep. LLM client construction is deferred
// per-tick so a transient credentials issue at daemon start does
// not permanently disable the sweeper.
func startMetaAnalysisSweeper(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) {
	interval := cfg.MetaAnalysis.SweepInterval.Or(defaultMetaAnalysisInterval)
	llmCfg := cli.LLMConfigFromFile(cfg.LLM)
	opts := cli.MetaAnalysisSweepOptionsFromConfig(cfg.MetaAnalysis)

	sw := &daemon.MetaAnalysisSweeper{
		Interval: interval,
		Log:      log,
		Sweep: func(sctx context.Context) error {
			return cli.RunMetaAnalysisSweep(sctx, st,
				func() (llm.Client, error) {
					return llm.FromConfig(sctx, llmCfg)
				},
				opts,
				daemon.DiscardWriter,
				daemon.DiscardWriter,
			)
		},
	}
	go sw.Run(ctx)
	log.Info("meta-analysis sweeper enabled",
		"interval", interval,
		"propose_cadence", opts.ProposeCadence,
		"reflect_cadence", opts.ReflectCadence,
		"challenge_cadence", opts.ChallengeCadence,
		"reflect_weekly_cadence", opts.ReflectWeeklyCadence,
		"skill_revision_cadence", opts.SkillRevisionCadence,
		"skill_revision_min_rate", opts.SkillRevisionMinRate)
}
