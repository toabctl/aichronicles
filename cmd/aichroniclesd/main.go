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

	"github.com/toabctl/aichronicles/internal/daemon"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aichroniclesd:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultDir, err := stateDir()
	if err != nil {
		return fmt.Errorf("resolve state dir: %w", err)
	}
	defaultSock := filepath.Join(defaultDir, "sock")
	defaultLog := filepath.Join(defaultDir, "events.jsonl")

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

	srv := daemon.NewServer(lg, logger)
	shutdown, err := daemon.ListenAndServe(*sockPath, srv.Handler())
	if err != nil {
		return err
	}
	logger.Info("aichroniclesd started", "socket", *sockPath, "log", *logPath)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("aichroniclesd shutting down")
	return shutdown()
}

// stateDir resolves $XDG_STATE_HOME/aichronicles, falling back to
// ~/.local/state/aichronicles when XDG_STATE_HOME is unset.
func stateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "aichronicles"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "aichronicles"), nil
}
