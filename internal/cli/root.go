package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Execute runs the root aichronicles command. Returned exit code is the
// caller's responsibility — the cobra errors surface via os.Stderr and
// non-zero exits on real CLI failures. The `ingest` subcommand inverts
// this: it returns nil on daemon errors to never block Claude's hook.
//
// We set SilenceErrors on the root so cobra does not double-print its
// default "Error: ..." line on top of our own messages, but that means
// we own the error-surfacing contract here. Print whatever the RunE
// returned to stderr so the user sees what failed instead of a bare
// exit-code-1.
func Execute() int {
	return executeCmd(newRootCmd(), os.Stderr)
}

// executeCmd is the testable body of Execute. Splitting it out lets
// tests assert on the printed error without shelling out to a real
// binary or juggling os.Stderr.
func executeCmd(root *cobra.Command, stderr io.Writer) int {
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "aichronicles:", err)
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
	cmd.AddCommand(newImportJSONLCmd())
	cmd.AddCommand(newImportClaudeTranscriptsCmd())
	cmd.AddCommand(newSearchCmd())
	cmd.AddCommand(newSessionsCmd())
	cmd.AddCommand(newAuditCmd())
	cmd.AddCommand(newScrubCmd())
	cmd.AddCommand(newSummarizeCmd())
	cmd.AddCommand(newReflectCmd())
	cmd.AddCommand(newProposeCmd())
	cmd.AddCommand(newMCPServeCmd())
	return cmd
}
