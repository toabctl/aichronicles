package web

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// defaultIdleTimeout is the auto-shutdown window applied when the
// server is socket-activated by systemd (LISTEN_FDS in env). 5 min
// balances "don't churn the service on rapid back-to-back tab
// loads" against "actually free resources when the user closes
// every tab". Tunable via --idle-timeout.
const defaultIdleTimeout = 5 * time.Minute

// NewCommand returns the `aichronicles web` cobra command. Lives
// in this package (not internal/cli) so the web feature is
// self-contained — the cli package just forwards to this
// constructor.
func NewCommand() *cobra.Command {
	var cfg Config
	var dbPath string
	var idleTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve a local web UI for browsing sessions and summaries",
		Long: "Starts a small HTTP server on localhost that lists captured\n" +
			"sessions, surfaces cached LLM summaries, and exposes the same\n" +
			"FTS5 search the CLI uses. Reads the SQLite store directly in\n" +
			"read-only mode — does not go through the daemon's UDS, does\n" +
			"not write.\n\n" +
			"Default bind is 127.0.0.1; pass --bind to change. Binding to\n" +
			"a non-loopback address surfaces a startup warning. The server\n" +
			"has no authentication; the localhost-only boundary is the\n" +
			"trust model, mirroring the daemon's 0600 UDS.\n\n" +
			"Socket activation: when launched by systemd via\n" +
			"aichronicles-web.socket (LISTEN_FDS in env), the server\n" +
			"adopts the inherited fd, ignores --bind/--port, and enables\n" +
			"idle auto-shutdown (default 5m of zero connections) so the\n" +
			"service exits between bursts and the .socket unit relaunches\n" +
			"it on the next request.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedDB, err := paths.ResolveStorePath(dbPath)
			if err != nil {
				return err
			}
			st, err := store.Open(resolvedDB)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(),
				&slog.HandlerOptions{Level: slog.LevelInfo})).With("cmd", "aichronicles web")

			// Socket activation takes priority over --bind/--port.
			// listenFromSystemd returns (nil, nil) when LISTEN_FDS
			// isn't set, so this is a no-op for terminal launches.
			ln, err := listenFromSystemd()
			if err != nil {
				return fmt.Errorf("systemd socket activation: %w", err)
			}
			if ln != nil {
				cfg.Listener = ln
				// Default to auto-shutdown under socket activation;
				// the user can still override via --idle-timeout=0
				// to disable it explicitly even when activated.
				if idleTimeout == 0 {
					idleTimeout = defaultIdleTimeout
				}
				log.Info("adopted systemd-passed listener",
					"addr", ln.Addr().String())
			}
			cfg.IdleTimeout = idleTimeout

			if isPublicBind(cfg.Bind) {
				log.Warn("binding to a non-loopback address — anyone on the network can read the corpus",
					"bind", cfg.Bind)
			}

			s := NewServer(st, cfg, log)

			// SIGINT / SIGTERM trigger graceful shutdown via
			// ctx cancellation. Run blocks until then.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			_, _ = fmt.Fprintf(cmd.OutOrStderr(),
				"aichronicles web: http://%s\n", s.Addr())
			return s.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&cfg.Bind, "bind", DefaultBind,
		"address to listen on (loopback by default; set to 0.0.0.0 for LAN access; ignored under systemd socket activation)")
	cmd.Flags().IntVar(&cfg.Port, "port", DefaultPort,
		"port to listen on (ignored under systemd socket activation)")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0,
		"shut down after this long with zero open connections (0 = no auto-shutdown when launched directly; defaults to 5m under systemd socket activation)")

	return cmd
}
