// aichronicles-api is the daemon: a Unix-socket HTTP server that
// serves /v1/* read+write JSON and /v1/stream SSE. Run via systemd
// --user with socket activation, or directly for development.
//
// Write-ownership contract — what touches SQLite from where:
//
//   - The daemon owns the INGEST PATH end-to-end: every write to
//     raw_envelopes, events, ingest_pending, extractions, sessions,
//     and session_outcomes goes through handleIngest → IngestWorker
//     → events.Pipeline.Process inside this process. The hook
//     subprocess and every import path are clients of /v1/ingest.
//
//   - Maintenance commands that REWRITE daemon-owned tables refuse
//     to run while the daemon is up. Today that's
//     `aichronicles backfill-extractions` (rewrites extractions);
//     `aichronicles scrub` runs through the daemon's POST /v1/scrub
//     instead of opening the store directly, so it inherits the
//     write lock.
//
//   - LLM-CACHE tables (llm_outputs, skill_candidates,
//     semantic_facts) have a documented second-writer flow: the
//     CLI subcommands `propose`, `propose add/merge/discard`,
//     `induction`, `summaries`, and `meta sweep` open the SQLite
//     file directly and INSERT their results. This is intentional —
//     funneling many-MB LLM outputs through the daemon's UDS one
//     row at a time would waste bandwidth and fight the daemon's
//     HTTP request budget, and UNIQUE constraints on the affected
//     tables turn race-condition duplicates into ON CONFLICT
//     idempotency rather than data corruption.
//
// The HTML web browser ships as a SEPARATE process (the
// `aichronicles web` subcommand of the main `aichronicles` CLI,
// installed as the aichronicles-web.{socket,service} systemd unit
// pair). Splitting it out keeps an HTML-template panic or memory
// leak from reaching the ingest worker — arch_review_2026_05_13
// HIGH #4. The web process reads via internal/apiclient against
// this daemon; it does not open SQLite directly.
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

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// defaultShutdownDrainTimeout caps how long the daemon will wait
// for in-flight requests to finish after SIGTERM / SIGINT.
// systemd's default TimeoutStopSec is 90s; 10s is comfortably
// under that while still letting a slow SQLite write commit.
// Operators override via [limits].shutdown_drain_timeout in the
// config file.
const defaultShutdownDrainTimeout = 10 * time.Second

// workerShutdownGrace bounds how long the main goroutine waits
// for the IngestWorker to finish its final drain after the
// listener has stopped accepting. Sized comfortably above the
// worker's internal shutdownBudget (api.defaultWorkerShutdownBudget
// = 5s) PLUS the worst-case Pipeline.Process for a single row
// (multi-MB envelope = redact + extractors + insert + FTS5
// indexing can take a few seconds). 20s leaves the worker time
// to finish a row that started just before ctx.Done and still
// drain the rest of the backlog. Independent of drainCtx so a
// slow listener-drain doesn't starve the worker.
//
// Bumped from 7s after arch_review_2026_05_13 MEDIUM #9 flagged
// that a row in flight at cancellation time could chew most of
// the 5s internal budget and leave near-zero for the rest of
// the queue.
const workerShutdownGrace = 20 * time.Second

func main() {
	if err := newRootCmd().Execute(); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("aichronicles-api failed", "err", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		sockFlag string
		dbFlag   string
	)
	cmd := &cobra.Command{
		Use:           "aichronicles-api",
		Short:         "JSON/SSE daemon for aichronicles (SQLite write-owner)",
		Long:          "aichronicles-api is the single SQLite-writing daemon. It serves /v1/* read+write JSON and /v1/stream SSE live activity over a Unix-domain socket. The hook subprocess (`aichronicles hook`) and every CLI subcommand are clients of the JSON surface; the HTML web browser ships as a separate process — `aichronicles web` / aichronicles-web.service — for blast-radius isolation.",
		Version:       cli.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(sockFlag, dbFlag)
		},
	}
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"unix socket path (overrides $AICHRONICLES_API_SOCKET; defaults to XDG_RUNTIME_DIR/aichronicles/api.sock)")
	cmd.Flags().StringVar(&dbFlag, "db", "",
		"SQLite store path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

func run(sockFlag, dbFlag string) error {
	resolvedSock, err := paths.ResolveAPISocketPath(sockFlag)
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
	// The api daemon is the sole migrator. Every other process
	// (web, CLI subcommands, MCP via apiclient) calls plain
	// store.Open and gets ErrSchemaTooOld if it starts before
	// the daemon has finished bringing the schema current.
	st, err := store.OpenMigrate(resolvedDB)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	st.SetMaxOpenConns(cfg.Limits.SQLiteMaxOpenConns)
	logger.Info("aichronicles-api schema",
		"version", store.LatestSchemaVersion())

	srv := api.NewServer(st, logger).
		WithMaxEnvelopeBytes(cfg.Limits.MaxEnvelopeBytes).
		WithIngestQueueMax(cfg.Limits.IngestQueueMax)
	defer srv.Close()

	var shutdown func(context.Context) error
	activationListener, err := api.ListenFromSystemd()
	if err != nil {
		return fmt.Errorf("systemd socket activation: %w", err)
	}
	var startMsg string
	if activationListener != nil {
		shutdown = api.Serve(activationListener, srv.Handler(), logger)
		startMsg = "socket-activated by systemd"
		logger.Info("aichronicles-api started (socket-activated by systemd)", "db", resolvedDB)
	} else {
		shutdown, err = api.ListenAndServe(resolvedSock, srv.Handler(), logger)
		if err != nil {
			return err
		}
		startMsg = "listener at " + resolvedSock
		logger.Info("aichronicles-api started", "socket", resolvedSock, "db", resolvedDB)
	}

	if err := notify.New(cfg.Notifications.DaemonStart).Send("aichronicles started", startMsg); err != nil {
		logger.Warn("start notification failed", "err", err)
	}

	// signal.NotifyContext gives us a cancellable context that
	// fires on SIGTERM/SIGINT. We use it purely to wait — the
	// actual drain deadline lives on a separate bounded child
	// below.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tell systemd we're up. Order matters: this MUST come after
	// the listener is bound, otherwise systemd considers us ready
	// while we're still starting and may fire READY-dependent
	// units against a not-yet-accepting daemon. No-op when not
	// under a notify-type service.
	api.NotifyReady(logger)

	// Start the watchdog AFTER READY=1 so the first probe goes
	// against an actually-up listener. Bound to sigCtx so a
	// shutdown signal stops the watchdog goroutine cleanly
	// alongside the rest of the daemon. No-op when WATCHDOG_USEC
	// is unset.
	if err := api.Start(sigCtx, resolvedSock, logger); err != nil {
		logger.Warn("start watchdog", "err", err)
	}

	// IngestWorker lifecycle is deliberately NOT bound to sigCtx:
	// the worker must outlive the listener so any envelope an
	// in-flight POST enqueues during the listener's drain pass
	// still makes it through redact + write. cancelWorker fires
	// AFTER the listener drain below; we then wait on Done()
	// before returning so the worker's shutdown drain finishes.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go func() { _ = srv.Worker().Run(workerCtx) }()

	<-sigCtx.Done()
	drainTimeout := cfg.Limits.ShutdownDrainTimeout.Or(defaultShutdownDrainTimeout)
	logger.Info("aichronicles-api shutting down", "drain_timeout", drainTimeout)

	// Tell systemd we're shutting down deliberately so it can
	// distinguish a clean exit from a watchdog-driven kill in the
	// journal. Issued before drain so it lands even if a pending
	// request takes the full drain window.
	api.NotifyStopping(logger)

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := shutdown(drainCtx); err != nil {
		logger.Warn("api shutdown", "err", err)
	}
	// Listener has drained; any envelopes accepted at the wire are
	// now committed to ingest_pending. Cancel the worker so its
	// shutdown drain finishes the redact+write side, and wait for
	// the goroutine to actually exit before returning so the
	// process doesn't tear down underneath an active transaction.
	//
	// The worker wait uses a FRESH budget (workerShutdownGrace),
	// not the already-spent drainCtx. drainCtx may have been
	// entirely consumed by a slow listener drain — without a
	// separate budget the worker would get zero time to flush
	// pending rows. The grace value is sized just over the
	// worker's internal shutdownBudget so a healthy worker
	// finishes cleanly and a wedged one is bounded.
	cancelWorker()
	workerGraceCtx, cancelGrace := context.WithTimeout(context.Background(), workerShutdownGrace)
	defer cancelGrace()
	select {
	case <-srv.Worker().Done():
	case <-workerGraceCtx.Done():
		logger.Warn("ingest worker did not drain within grace period",
			"grace", workerShutdownGrace)
	}

	// Audit log: how many rows are still in ingest_pending at
	// exit? Anything > 0 here means an envelope landed during
	// the listener-drain window but wasn't processed before the
	// worker's budget expired (or its parent shutdown got cut
	// short). Those rows persist in SQLite and will be picked up
	// by the worker's initial drain on the next daemon start —
	// no event loss, just delayed processing. Surfacing the
	// count makes that visible to the operator.
	if pending, err := store.CountPending(context.Background(), st.DB()); err == nil && pending > 0 {
		logger.Warn("ingest_pending leftover at shutdown — will resume on next start",
			"rows", pending)
	}
	return nil
}
