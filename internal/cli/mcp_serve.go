package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/mcp"
	"github.com/toabctl/aichronicles/internal/paths"
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
		Short: "Run an MCP server over stdio exposing the user's session history",
		Long: "Starts a Model Context Protocol server on stdin/stdout that\n" +
			"lets a model query the user's PAST Claude Code / Gemini CLI\n" +
			"sessions. All tools read the local SQLite store; nothing\n" +
			"writes back.\n\n" +
			"Tools exposed:\n" +
			"  search_events        — keyword search over past events\n" +
			"  list_sessions        — recent past conversations\n" +
			"  find_episodes        — episodic recall (intent-keyed slices of past sessions)\n" +
			"  get_summary          — cached summary of one session\n" +
			"  list_subagents       — sub-agent threads inside a session\n" +
			"  get_insights         — usage report (top tools / skills / activity)\n" +
			"  list_skills          — installed + invoked skills\n" +
			"  get_skill_staleness  — skills correlated with tool failures\n" +
			"  search_with_summary  — LLM-synthesised answer (requires API key)\n\n" +
			"Registered automatically by `aichronicles setup claude-code` under\n" +
			"the mcpServers.aichronicles entry of ~/.claude/settings.json.\n\n" +
			"Logs go to stderr as structured records so the host's MCP log\n" +
			"window surfaces them. Stdin close (client disconnect) ends the\n" +
			"process cleanly.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Resolve the store path for the startup log; we
			// don't actually open the store here. After the
			// MCP migration off RegisterAichroniclesTools, every
			// MCP tool reads through apiclient against the
			// aichronicles-api daemon's UDS — the daemon is the
			// only process that opens the SQLite file. This
			// matches the "out-of-process readers go through the
			// wire" policy codified in tools/depcheck.
			resolved, err := paths.ResolveStorePath(dbPath)
			if err != nil {
				return err
			}

			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(),
				&slog.HandlerOptions{Level: slog.LevelInfo})).With("cmd", "aichronicles mcp-serve")

			apiC, err := openAPIClient("")
			if err != nil {
				return err
			}

			srv := mcp.New(mcp.ServerInfo{
				Name:    mcpServerName,
				Version: mcpServerVersion,
			}, log)
			mcp.RegisterAichroniclesAnalyticsTools(srv, apiC)
			mcp.RegisterAichroniclesAPITools(srv, apiC)

			// Register LLM-backed tools (search_with_summary) only
			// when the user has an API key configured — otherwise
			// the tools are omitted from the catalog entirely so an
			// agent doesn't see them advertised and call expecting
			// them to work.
			cfg, cfgErr := config.Load()
			if cfgErr == nil {
				llmCfg := LLMConfigFromFile(cfg.LLM)
				mcp.RegisterAichroniclesLLMTools(srv, apiC,
					func() (llm.Client, error) { return llm.FromConfig(cmd.Context(), llmCfg) })
			} else {
				log.Warn("mcp: skipping LLM-backed tools (no config)", "err", cfgErr)
			}

			log.Info("mcp server starting",
				"protocol", mcp.ProtocolVersion,
				"db", resolved,
			)
			return srv.Run(cmd.Context(), cmd.InOrStdin(), os.Stdout)
		},
	}
	addDBFlag(cmd, &dbPath)
	return cmd
}
