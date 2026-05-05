package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/pkg/api"
)

// sessionCompletionLimit caps how many candidates the completion
// helpers return. Big enough to surface real history; small enough
// that a blank-tab on a busy store doesn't flood the shell with
// thousands of rows.
const sessionCompletionLimit = 50

// completeSessionID is a cobra completion function that returns
// matching session ids from the live api, formatted as
// "id\tdescription" so shells (zsh, fish) render the cwd + first
// prompt next to each candidate. Wired into both
// ValidArgsFunction (for positional <session> args) and
// RegisterFlagCompletionFunc (for --session flag).
//
// Errors swallow to NoFileComp + Error — completion frameworks
// handle the directive; emitting a Go error string into the
// shell output would mostly produce noise. The empty-string flag
// passed to openAPIClient picks up $AICHRONICLES_API_SOCKET if set,
// or the XDG default — same resolution every other CLI subcommand
// uses.
func completeSessionID(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	c, err := openAPIClient("")
	if err != nil {
		return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
	}
	return completeSessionIDFrom(cmd, c, toComplete)
}

// completeSessionIDFrom is the inner shape that takes an explicit
// apiclient.Client. Tests construct one against a httptest server
// (apiForStore) and call this directly, bypassing the cobra-time
// socket resolution.
func completeSessionIDFrom(cmd *cobra.Command, c interface {
	SessionsForCompletion(ctx context.Context, prefix string, limit int) (api.SessionCompletionsResponse, error)
}, toComplete string) ([]string, cobra.ShellCompDirective) {
	resp, err := c.SessionsForCompletion(cmd.Context(), toComplete, sessionCompletionLimit)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError | cobra.ShellCompDirectiveNoFileComp
	}

	out := make([]string, 0, len(resp.Sessions))
	for _, r := range resp.Sessions {
		// cobra splits "id\tdescription" automatically: the
		// part before \t becomes the candidate string the
		// shell substitutes; the part after is the description
		// shells with rich output (zsh, fish) display.
		out = append(out, r.ID+"\t"+r.Description)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// registerSessionFlagCompletion is sugar for cmd.RegisterFlagCompletionFunc("session", completeSessionID).
// Lives here so call sites stay one-line and the cobra error
// (which can only fire if the named flag isn't registered yet)
// gets a single tested handler.
func registerSessionFlagCompletion(cmd *cobra.Command) {
	// The "session" flag must be registered first; if it isn't,
	// cobra returns an error here. Panicking is the right shape:
	// it's a wiring bug, not a user-facing condition.
	if err := cmd.RegisterFlagCompletionFunc("session", completeSessionID); err != nil {
		panic("cli: completeSessionID requires --session flag to exist on " + cmd.Use + ": " + err.Error())
	}
}
