package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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
	return store.Scrub(legacyScrubCtx(), s.DB(), scanner, opts)
}

func newScrubCmd() *cobra.Command {
	var (
		yes      bool
		sockFlag string
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
			"backup of the DB file first if you care about forensics.\n\n" +
			"Talks to aichronicles-api over its UDS so the scrub holds the\n" +
			"single SQLite writer lock cleanly (no contention with the live\n" +
			"ingest path).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			resp, err := c.Scrub(cmd.Context(), wire.ScrubRequest{DryRun: !yes})
			if err != nil {
				return fmt.Errorf("scrub: %w", err)
			}
			renderScrubResponse(cmd.OutOrStdout(), resp)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm irreversible writes (required to mutate the DB)")
	addSocketFlag(cmd, &sockFlag)
	return cmd
}

// renderScrubResponse prints the wire.ScrubResponse in the same
// summary shape store.Scrub used to emit through its Out writer
// — one summary line plus a sorted pattern-hit table. Same shape
// keeps existing operator habits and any wrapping scripts intact.
func renderScrubResponse(out io.Writer, r wire.ScrubResponse) {
	_, _ = fmt.Fprintf(out,
		"scanned=%d envelopes_rewritten=%d events_content_rewritten=%d "+
			"llm_outputs_scanned=%d llm_outputs_rewritten=%d dry_run=%t\n",
		r.EventsScanned, r.EnvelopesRewritten, r.EventsRewritten,
		r.LLMOutputsScanned, r.LLMOutputsRewritten, r.DryRun)
	keys := make([]string, 0, len(r.PatternHits))
	for k := range r.PatternHits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		_, _ = fmt.Fprintf(out, "  %-24s %d\n", p, r.PatternHits[p])
	}
}
