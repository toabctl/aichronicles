// aichronicles-api is the unified daemon: a Unix-socket HTTP server
// that serves /v1/* read+write JSON, /v1/stream SSE, and the web
// HTML browser. It is the single SQLite-handling process. Run via
// systemd --user with socket activation, or directly for
// development.
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
		Short:         "Unified read/write/SSE/web daemon for aichronicles",
		Long:          "aichronicles-api is the single SQLite-owning daemon. It serves the /v1/* read+write API, /v1/stream SSE live activity, and the web HTML browser over a Unix-domain socket. The hook subprocess (`aichronicles hook`) and every CLI subcommand are clients of this surface.",
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
	st, err := store.Open(resolvedDB)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	st.SetMaxOpenConns(cfg.Limits.SQLiteMaxOpenConns)

	srv := api.NewServer(st, logger).WithMaxEnvelopeBytes(cfg.Limits.MaxEnvelopeBytes)

	var shutdown func(context.Context) error
	activationListener, err := api.ListenFromSystemd()
	if err != nil {
		return fmt.Errorf("systemd socket activation: %w", err)
	}
	var startMsg string
	if activationListener != nil {
		shutdown = api.Serve(activationListener, srv.Handler())
		startMsg = "socket-activated by systemd"
		logger.Info("aichronicles-api started (socket-activated by systemd)", "db", resolvedDB)
	} else {
		shutdown, err = api.ListenAndServe(resolvedSock, srv.Handler())
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
	return shutdown(drainCtx)
}
