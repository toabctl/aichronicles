package web

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// NewCommand returns the `aichronicles web` cobra command. Lives
// in this package (not internal/cli) so the web feature is
// self-contained — the cli package just forwards to this
// constructor.
func NewCommand() *cobra.Command {
	var cfg Config
	var dbPath string

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
			"trust model, mirroring the daemon's 0600 UDS.",
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
		"address to listen on (loopback by default; set to 0.0.0.0 for LAN access)")
	cmd.Flags().IntVar(&cfg.Port, "port", DefaultPort,
		"port to listen on")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")

	return cmd
}
