// aichroniclesd is the ingest daemon: a Unix-socket HTTP server that
// accepts envelopes on POST /v1/ingest and appends them to an on-disk
// JSONL event log.
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
	defaultLog, err := paths.EventLog()
	if err != nil {
		return fmt.Errorf("resolve event log path: %w", err)
	}

	sockPath := flag.String("socket", defaultSock, "unix socket path")
	logPath := flag.String("log", defaultLog, "append-only JSONL event log path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := os.MkdirAll(filepath.Dir(*logPath), 0o700); err != nil {
		return fmt.Errorf("ensure log dir: %w", err)
	}
	lg, err := daemon.OpenLogger(*logPath)
	if err != nil {
		return err
	}
	defer func() { _ = lg.Close() }()

	cfg, err := config.Load()
	if err != nil {
		logger.Warn("config load failed, using defaults", "err", err)
		d := config.Default()
		cfg = &d
	}

	srv := daemon.NewServer(lg, logger)

	var shutdown func() error
	activationListener, err := daemon.ListenFromSystemd()
	if err != nil {
		return fmt.Errorf("systemd socket activation: %w", err)
	}
	var startMsg string
	if activationListener != nil {
		shutdown = daemon.Serve(activationListener, srv.Handler())
		startMsg = "socket-activated by systemd"
		logger.Info("aichroniclesd started (socket-activated by systemd)", "log", *logPath)
	} else {
		shutdown, err = daemon.ListenAndServe(*sockPath, srv.Handler())
		if err != nil {
			return err
		}
		startMsg = "listener at " + *sockPath
		logger.Info("aichroniclesd started", "socket", *sockPath, "log", *logPath)
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
