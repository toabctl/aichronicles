package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/mcp"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// mcpServerName is what the MCP `initialize` handshake reports back
// to the client. Shown in Claude Desktop's tool picker next to the
// tool names.
const mcpServerName = "aichronicles"

// mcpServerVersion is reported alongside the name. Static for now;
// if we add a --version plumbing later, wire it here.
const mcpServerVersion = "0.1.0"

func newMCPServeCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "mcp-serve",
		Short: "Run an MCP server over stdio exposing aichronicles data",
		Long: "Starts a Model Context Protocol server on stdin/stdout,\n" +
			"offering three read-only tools (search_events, list_sessions,\n" +
			"get_summary) backed by the local SQLite store.\n\n" +
			"Typically registered in Claude Desktop's mcp_servers section:\n\n" +
			"    \"aichronicles\": {\n" +
			"      \"command\": \"/home/you/.local/bin/aichronicles\",\n" +
			"      \"args\": [\"mcp-serve\"]\n" +
			"    }\n\n" +
			"Logs are emitted as structured records on stderr so Claude\n" +
			"Desktop's own log window surfaces them. Stdin close (client\n" +
			"disconnect) ends the process cleanly.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := paths.ResolveStorePath(dbPath)
			if err != nil {
				return err
			}
			s, err := store.Open(resolved)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(),
				&slog.HandlerOptions{Level: slog.LevelInfo})).With("cmd", "aichronicles mcp-serve")

			srv := mcp.New(mcp.ServerInfo{
				Name:    mcpServerName,
				Version: mcpServerVersion,
			}, log)
			mcp.RegisterAichroniclesTools(srv, s)

			log.Info("mcp server starting",
				"protocol", mcp.ProtocolVersion,
				"db", resolved,
			)
			return srv.Run(cmd.Context(), cmd.InOrStdin(), os.Stdout)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}
