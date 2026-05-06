package cli

import (
	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
)

// newMetaCmd is the umbrella for cadence-gated meta-analyses
// (propose / reflect / challenge / reflect_weekly /
// skill_revision). The single child today is `sweep`, which
// fires whichever kinds are overdue per [meta_analysis] in the
// config file. It is the systemd-timer-driven entry point that
// replaces the daemon-resident sweeper goroutine: stateless,
// idempotent, and safe to drive from a `*.timer` unit.
func newMetaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta",
		Short: "Cadence-gated meta-analyses (propose / reflect / challenge / reflect_weekly / skill_revision)",
		Long: "meta is the umbrella for time-driven analyses that fire\n" +
			"on a per-kind cadence rather than per-session. The cadence\n" +
			"gate runs against the persisted last-fired timestamp in\n" +
			"SQLite (kind=propose, kind=reflect, etc.), so a missed\n" +
			"timer firing is automatically picked up on the next run.\n\n" +
			"Subcommands:\n" +
			"  sweep — fire every overdue kind in one pass\n",
	}
	cmd.AddCommand(newMetaSweepCmd())
	return cmd
}

// newMetaSweepCmd wraps RunMetaAnalysisSweep — the same orchestrator
// the daemon's MetaAnalysisSweeper goroutine wraps. Pulling this out
// to a CLI subcommand lets a systemd `--user` timer drive the work,
// which gives operators per-run journal isolation, OnUnitInactiveSec
// catch-up after suspend, and the option to disable sweeps without
// touching the daemon. Per-kind failure isolation and the empty-
// window-isn't-an-error semantics live in RunMetaAnalysisSweep.
func newMetaSweepCmd() *cobra.Command {
	var (
		dbPath   string
		sockFlag string
	)
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Fire every overdue meta-analysis kind in one pass",
		Long: "Walks the cadence-gated kinds (propose / reflect /\n" +
			"challenge / reflect_weekly / skill_revision) and dispatches\n" +
			"any whose persisted last-fired timestamp is older than the\n" +
			"configured cadence. Cadences and per-kind skip flags come\n" +
			"from [meta_analysis] in the config file; defaults match\n" +
			"the prompts' natural horizons (24h / 7d).\n\n" +
			"Per-kind failure isolation: a propose failure does not\n" +
			"skip the week's reflect digest. The first non-nil error\n" +
			"is returned, but every eligible kind is attempted before\n" +
			"the command exits.\n\n" +
			"Designed to be driven from a systemd --user timer (see\n" +
			"the install assets). Manual invocation works too — useful\n" +
			"when forcing one round outside the cadence.\n\n" +
			"Requires " + llm.APIKeyEnv + ".",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)
			opts := MetaAnalysisSweepOptionsFromConfig(cfg.MetaAnalysis)

			ctx := cmd.Context()
			return RunMetaAnalysisSweep(ctx, s, c,
				func() (llm.Client, error) { return llm.FromConfig(ctx, llmCfg) },
				opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	return cmd
}
