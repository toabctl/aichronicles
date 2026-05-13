package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/paths"
)

// addSocketFlag wires the canonical --socket flag onto cmd. Every
// CLI subcommand that talks to aichronicles-api needs the same
// flag with the same help text; the help string lived in 30+
// places verbatim and would silently drift when any one site got
// updated.
func addSocketFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
}

// openAPIClient resolves the api socket path (flag → env →
// XDG default) and returns a configured client. Used by every
// migrated CLI subcommand that talks to aichronicles-api.
//
// Failure modes:
//   - flag is set but path resolution fails (e.g. nil XDG fallback): error
//   - socket does not exist / daemon not running: deferred — surfaces on
//     the first request as apiclient.ErrSocketUnavailable, with the
//     resolved path in the error message
func openAPIClient(sockFlag string) (*apiclient.Client, error) {
	resolved, err := paths.ResolveAPISocketPath(sockFlag)
	if err != nil {
		return nil, fmt.Errorf("resolve api socket: %w", err)
	}
	return apiclient.NewClient(resolved), nil
}
