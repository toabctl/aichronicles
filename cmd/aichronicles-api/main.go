// aichronicles-api is the unified daemon: a Unix-socket HTTP server
// that serves /v1/* read+write JSON, /v1/stream SSE, and the web
// HTML browser. It is the single SQLite-handling process. Run via
// systemd --user with socket activation, or directly for
// development.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/toabctl/aichronicles/internal/web"
)

// defaultWebBind / defaultWebPort match what the standalone
// `aichronicles web` command shipped before the fold-in. The web
// daemon is loopback-only by design — the trust model is the
// localhost boundary, mirroring the JSON socket's 0600 fs perms.
const (
	defaultWebBind = "127.0.0.1"
	defaultWebPort = 7474
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
		sockFlag    string
		dbFlag      string
		webBind     string
		webPort     int
		webDisabled bool
	)
	cmd := &cobra.Command{
		Use:           "aichronicles-api",
		Short:         "Unified read/write/SSE/web daemon for aichronicles",
		Long:          "aichronicles-api is the single SQLite-owning daemon. It serves the /v1/* read+write API + /v1/stream SSE live activity over a Unix-domain socket, and the HTML web browser on a localhost TCP port. The hook subprocess (`aichronicles hook`) and every CLI subcommand are clients of the JSON surface; humans browse the HTML surface directly.",
		Version:       cli.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(sockFlag, dbFlag, webBind, webPort, webDisabled)
		},
	}
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"unix socket path (overrides $AICHRONICLES_API_SOCKET; defaults to XDG_RUNTIME_DIR/aichronicles/api.sock)")
	cmd.Flags().StringVar(&dbFlag, "db", "",
		"SQLite store path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().StringVar(&webBind, "web-bind", defaultWebBind,
		"address for the HTML web UI (loopback by default; non-loopback prints a warning)")
	cmd.Flags().IntVar(&webPort, "web-port", defaultWebPort,
		"port for the HTML web UI; 0 picks an ephemeral port (the daemon logs the bound address)")
	cmd.Flags().BoolVar(&webDisabled, "no-web", false,
		"disable the HTML web UI entirely (JSON / SSE socket still served)")
	return cmd
}

func run(sockFlag, dbFlag, webBind string, webPort int, webDisabled bool) error {
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
	defer srv.Close()

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

	// Web HTML surface: bound to localhost TCP so a browser can hit
	// it without proxying through the UDS. Reads the same *store.Store
	// the JSON api uses — single SQLite handle, no second writer
	// path. Runs as a goroutine so the main shutdown path waits on
	// both the JSON UDS drain and the TCP web drain.
	var webShutdown func(context.Context) error
	if !webDisabled {
		var wsErr error
		webShutdown, wsErr = startWebListener(webBind, webPort, st, logger)
		if wsErr != nil {
			logger.Warn("web UI disabled", "err", wsErr)
		}
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
	if webShutdown != nil {
		// Drain the web TCP listener before the api UDS so any
		// in-flight HTML request that is reading the store finishes
		// before the api shuts down. Errors are logged but don't
		// block the api drain — losing one in-flight HTML request
		// on shutdown is preferable to leaking the api drain.
		if err := webShutdown(drainCtx); err != nil {
			logger.Warn("web shutdown", "err", err)
		}
	}
	return shutdown(drainCtx)
}

// startWebListener binds a TCP listener for the HTML web UI and
// starts an http.Server in a goroutine serving web.Server's
// handler. Returns a shutdown func that gracefully drains in-flight
// browser requests.
//
// The web reads the same *store.Store the JSON api uses, so there
// is no second writer path — the single-writer invariant survives
// even when both surfaces are live in the same process.
//
// Loopback is the default and the trust boundary; binding to a
// non-loopback address logs a warning but does not refuse — the
// operator may know what they're doing (e.g. a private LAN).
func startWebListener(bind string, port int, st *store.Store, logger *slog.Logger) (func(context.Context) error, error) {
	if bind == "" {
		bind = defaultWebBind
	}
	addr := fmt.Sprintf("%s:%d", bind, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	if !isLoopbackBind(bind) {
		logger.Warn("web UI bound to non-loopback address — anyone on the network can read the corpus",
			"addr", ln.Addr().String())
	}

	wsrv := web.NewServer(st, web.Config{Bind: bind, Port: port}, logger.With("component", "web"))
	httpSrv := &http.Server{
		Handler:           wsrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("web Serve", "err", err)
		}
	}()
	logger.Info("aichronicles web listening", "addr", ln.Addr().String())
	return httpSrv.Shutdown, nil
}

// isLoopbackBind reports whether bind names a loopback address.
// Returns true for the empty string (treated as default loopback)
// and any address that net.ParseIP recognises as a loopback.
func isLoopbackBind(bind string) bool {
	if bind == "" || bind == "localhost" {
		return true
	}
	if ip := net.ParseIP(bind); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
