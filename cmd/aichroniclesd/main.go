// aichroniclesd is the ingest daemon: a Unix-socket HTTP server that
// accepts envelopes on POST /v1/ingest and persists them to SQLite.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// defaultShutdownDrainTimeout caps how long the daemon will wait for
// in-flight requests to finish after SIGTERM / SIGINT. systemd's
// default TimeoutStopSec is 90s; 10s is comfortably under that while
// still letting a slow SQLite write commit. Operators can override
// via [limits].shutdown_drain_timeout in the config file.
const defaultShutdownDrainTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("aichroniclesd failed to start", "err", err)
		os.Exit(1)
	}
}

func run() error {
	defaultSock, err := paths.Socket()
	if err != nil {
		return fmt.Errorf("resolve socket path: %w", err)
	}
	defaultDB, err := paths.StorePath()
	if err != nil {
		return fmt.Errorf("resolve store path: %w", err)
	}

	sockPath := flag.String("socket", defaultSock, "unix socket path")
	dbPath := flag.String("db", defaultDB, "SQLite store path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Warn("config load failed, using defaults", "err", err)
		d := config.Default()
		cfg = &d
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return fmt.Errorf("ensure store dir: %w", err)
	}
	st, err := store.Open(*dbPath)
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
		logger.Info("aichroniclesd started (socket-activated by systemd)", "db", *dbPath)
	} else {
		shutdown, err = daemon.ListenAndServe(*sockPath, srv.Handler())
		if err != nil {
			return err
		}
		startMsg = "listener at " + *sockPath
		logger.Info("aichroniclesd started", "socket", *sockPath, "db", *dbPath)
	}

	if err := notify.New(cfg.Notifications.DaemonStart).Send("aichronicles started", startMsg); err != nil {
		logger.Warn("start notification failed", "err", err)
	}

	// signal.NotifyContext gives us a cancellable context that fires
	// on SIGTERM/SIGINT. We use it purely to wait — the actual drain
	// deadline lives on a separate bounded child below.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()
	drainTimeout := cfg.Limits.ShutdownDrainTimeout.Or(defaultShutdownDrainTimeout)
	logger.Info("aichroniclesd shutting down", "drain_timeout", drainTimeout)

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return shutdown(drainCtx)
}
