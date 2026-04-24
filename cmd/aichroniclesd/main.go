// aichroniclesd is the ingest daemon: a Unix-socket HTTP server that
// accepts envelopes on POST /v1/ingest and persists them to SQLite.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/daemon"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

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

	srv := daemon.NewServer(st, logger)

	var shutdown func() error
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

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("aichroniclesd shutting down")
	return shutdown()
}
