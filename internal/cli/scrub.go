package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// Re-exports of the store package's scrub types so tests and any
// external caller that imported the legacy cli.Scrub* names keep
// working after the logic moved to internal/store. New code should
// import internal/store directly.
type (
	ScrubOptions = store.ScrubOptions
	ScrubReport  = store.ScrubReport
)

// RunScrub is the legacy alias kept for cli tests. New code calls
// store.Scrub(ctx, db, scanner, opts) directly.
func RunScrub(s *store.Store, scanner redact.Scanner, opts ScrubOptions, out io.Writer) (*ScrubReport, error) {
	if out != nil {
		opts.Out = out
	}
	// Tests rely on cancellation via the standard context API;
	// pass the legacy callers' best fallback. Scrub honors ctx
	// for query cancellation; callers that need a real context
	// should switch to store.Scrub.
	return store.Scrub(legacyScrubCtx(), s.DB(), scanner, opts)
}

func newScrubCmd() *cobra.Command {
	var (
		yes    bool
		dbPath string
	)
	cmd := &cobra.Command{
		Use:   "scrub",
		Short: "Rewrite stored events to remove credentials (IRREVERSIBLE with --yes)",
		Long: "Retroactive scrubber. For every stored event, runs the current\n" +
			"detectors and rewrites matches to <redacted:kind> markers in both\n" +
			"events.content_text and raw_envelopes.envelope_json.\n\n" +
			"Runs in dry-run mode by default: it reports what would change\n" +
			"without touching the database. Pass --yes to actually write.\n\n" +
			"This is IRREVERSIBLE. raw_envelopes is aichronicles' source-of-\n" +
			"truth layer; once rewritten, the original bytes are gone. Take a\n" +
			"backup of the DB file first if you care about forensics.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			_, err = store.Scrub(cmd.Context(), s.DB(), redact.Default(), store.ScrubOptions{
				DryRun: !yes,
				Out:    cmd.OutOrStdout(),
			})
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm irreversible writes (required to mutate the DB)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}
