package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// Execute runs the root aichronicles command. Returned exit code is the
// caller's responsibility — the cobra errors surface via os.Stderr and
// non-zero exits on real CLI failures. The `ingest` subcommand inverts
// this: it returns nil on daemon errors to never block Claude's hook.
func Execute() int {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "aichronicles",
		Short:         "Capture AI coding agent session events",
		Long:          "aichronicles is the client binary for the aichroniclesd ingest daemon. It receives hook payloads, wraps them in the canonical Envelope, and forwards to the daemon over a Unix domain socket.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	cmd.AddCommand(newIngestCmd())
	cmd.AddCommand(newSetupCmd())
	cmd.AddCommand(newTeardownCmd())
	return cmd
}
