package cli

import (
	"fmt"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/paths"
)

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
