package web

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/paths"
)

// defaultIdleTimeout is the auto-shutdown window applied when the
// server is socket-activated by systemd (LISTEN_FDS in env). 5 min
// balances "don't churn the service on rapid back-to-back tab
// loads" against "actually free resources when the user closes
// every tab". Tunable via --idle-timeout.
const defaultIdleTimeout = 5 * time.Minute

// NewCommand returns the `aichronicles web` cobra command.
//
// The web UI runs as its own process — separate from the
// aichronicles-api daemon — so an HTML-template panic or a
// memory-hungry view query can't tear down the ingest worker.
// Operators get the pair via `aichronicles setup systemd`:
// aichronicles-api.{socket,service} for the write/UDS surface,
// aichronicles-web.{socket,service} for the loopback-TCP HTML
// surface. SQLite WAL handles concurrent readers; the api
// remains the canonical writer.
func NewCommand() *cobra.Command {
	var cfg Config
	var sockPath string
	var idleTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve a local web UI for browsing sessions and summaries",
		Long: "Starts a small HTTP server on localhost that lists captured\n" +
			"sessions, surfaces cached LLM summaries, and exposes the same\n" +
			"FTS5 search the CLI uses. Reads pass through the aichronicles-api\n" +
			"daemon's UDS via internal/apiclient — the daemon stays the only\n" +
			"process that opens the SQLite file. Runs as its own service\n" +
			"(aichronicles-web.service) so a wedged template or runaway view\n" +
			"query can't tear down the ingest worker.\n\n" +
			"Default bind is 127.0.0.1; pass --bind to change. Binding to\n" +
			"a non-loopback address surfaces a startup warning.\n\n" +
			"There is NO authentication. Be precise about what the bind\n" +
			"does and does not buy: it stops other machines, but a\n" +
			"loopback TCP port is readable by every local user, unlike\n" +
			"the daemon's 0600 UDS which the kernel restricts to one uid.\n" +
			"On a shared host, treat the corpus as readable by anyone\n" +
			"with an account. Requests are additionally Host-checked in\n" +
			"process, so a web page you visit cannot reach the UI by\n" +
			"re-resolving its own hostname to 127.0.0.1.\n\n" +
			"Socket activation: when launched by systemd via\n" +
			"aichronicles-web.socket (LISTEN_FDS in env), the server\n" +
			"adopts the inherited fd, ignores --bind/--port, and enables\n" +
			"idle auto-shutdown (default 5m of zero connections) so the\n" +
			"service exits between bursts and the .socket unit relaunches\n" +
			"it on the next request.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedSock, err := paths.ResolveAPISocketPath(sockPath)
			if err != nil {
				return err
			}
			apiC := apiclient.NewClient(resolvedSock)

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

			s := NewServer(apiC, cfg, log)
			log.Info("aichronicles-web dialing api", "socket", resolvedSock)

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
	cmd.Flags().StringVar(&sockPath, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET; defaults to $XDG_RUNTIME_DIR/aichronicles/api.sock)")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0,
		"shut down after this long with zero open connections (0 = no auto-shutdown when launched directly; defaults to 5m under systemd socket activation)")

	return cmd
}
