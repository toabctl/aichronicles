package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
)

func newImportJSONLCmd() *cobra.Command {
	var sockFlag string
	cmd := &cobra.Command{
		Use:   "import-jsonl <path>",
		Short: "Replay events.jsonl into the SQLite store via aichronicles-api",
		Long: "Streams a JSONL file of ingest envelopes (typically the POC's\n" +
			"events.jsonl) into POST /v1/import on aichronicles-api.\n" +
			"Idempotent: duplicates (by event_id) are counted and skipped.\n\n" +
			"Use this once after upgrading from the JSONL-only POC to backfill\n" +
			"historical events into SQLite, or to replay a backup.\n\n" +
			"Trust model: the api applies server-side redaction to every line\n" +
			"regardless of any redaction.applied claim, so a third-party\n" +
			"events.jsonl can be imported safely. After import, run\n" +
			"`aichronicles audit` to inspect anything the redactor missed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("open %s: %w", args[0], err)
			}
			defer func() { _ = f.Close() }()

			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}

			report, err := ImportJSONL(cmd.Context(), c, f, args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	return cmd
}

// ImportReport summarizes an ImportJSONL run. Fields are
// preserved across the apiclient migration so existing callers
// and the human renderer keep working; the values now come from
// api.ImportStats rather than a local BufferedSink.
type ImportReport struct {
	Source     string
	Imported   int
	Deduped    int
	Invalid    int
	LinesRead  int
	DurationMS int64
}

func (r ImportReport) String() string {
	var b strings.Builder
	if r.Source != "" {
		fmt.Fprintf(&b, "source:       %s\n", r.Source)
	}
	fmt.Fprintf(&b, "lines read:   %d\n", r.LinesRead)
	fmt.Fprintf(&b, "imported:     %d\n", r.Imported)
	fmt.Fprintf(&b, "deduped:      %d (already in store by event_id)\n", r.Deduped)
	fmt.Fprintf(&b, "invalid:      %d (bad JSON or failed envelope validation)\n", r.Invalid)
	fmt.Fprintf(&b, "duration_ms:  %d", r.DurationMS)
	return b.String()
}

// ImportJSONL streams r into POST /v1/import. The api applies
// server-side redaction, runs the extractor registry, and writes
// through the SQLite Sink — same path as live ingest. Idempotent
// on event_id; per-line failures (malformed JSON, validation)
// increment Invalid and the run continues.
func ImportJSONL(ctx context.Context, c *apiclient.Client, r io.Reader, source string) (ImportReport, error) {
	stats, err := c.Import(ctx, r)
	if err != nil {
		return ImportReport{Source: source}, err
	}
	return ImportReport{
		Source:     source,
		LinesRead:  stats.LinesRead,
		Imported:   stats.Imported,
		Deduped:    stats.Deduped,
		Invalid:    stats.Invalid,
		DurationMS: stats.DurationM,
	}, nil
}
