package cli

import (
	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// addDBFlag wires the canonical --db flag onto cmd. Used by every
// CLI subcommand that opens the SQLite store directly (mostly the
// LLM-batch commands that talk to llm_outputs for caching). The
// help text is the contract for the env var precedence — pinning
// it here keeps the help consistent across commands.
func addDBFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
}

// openStore is the canonical "resolve --db flag, open the store"
// helper every CLI subcommand goes through. CLI flag wins over
// $AICHRONICLES_DB which wins over the XDG default; see
// paths.ResolveStorePath. Callers must defer s.Close().
//
// Centralised so a) every command shares one error path, and
// b) future instrumentation (open-time logging, metrics) lands
// in one place rather than 30.
func openStore(dbFlag string) (*store.Store, error) {
	s, _, err := openStoreResolved(dbFlag)
	return s, err
}

// openStoreResolved is openStore plus the resolved path. Use when
// the caller needs to log "opened DB at /…" or otherwise echo the
// effective path to the user (mcp_serve does this for telemetry).
func openStoreResolved(dbFlag string) (*store.Store, string, error) {
	resolved, err := paths.ResolveStorePath(dbFlag)
	if err != nil {
		return nil, "", err
	}
	s, err := store.Open(resolved)
	if err != nil {
		return nil, "", err
	}
	return s, resolved, nil
}
