package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/redact"
)

func newImportJSONLCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "import-jsonl <path>",
		Short: "Replay events.jsonl into the SQLite store",
		Long: "Reads a JSONL file of ingest envelopes (typically the POC's\n" +
			"events.jsonl) and inserts each line into the store. Idempotent:\n" +
			"duplicates (by event_id) are counted and skipped.\n\n" +
			"Use this once after upgrading from the JSONL-only POC to backfill\n" +
			"historical events into SQLite, or to replay a backup.\n\n" +
			"Trust model: import-jsonl bypasses the daemon. The store still\n" +
			"refuses unredacted envelopes (ErrRedactionRequired), but anything\n" +
			"the daemon would otherwise enforce — future origin signing, rate\n" +
			"limits, audit logging — does not run. Treat the input file as\n" +
			"authoritative; if a third party hands you events.jsonl, audit it\n" +
			"with `aichronicles audit` after import.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("open %s: %w", args[0], err)
			}
			defer func() { _ = f.Close() }()

			report, err := ImportJSONL(cmd.Context(), f, s)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

// ImportReport summarizes an ImportJSONL run.
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
	fmt.Fprintf(&b, "lines read:   %d\n", r.LinesRead)
	fmt.Fprintf(&b, "imported:     %d\n", r.Imported)
	fmt.Fprintf(&b, "deduped:      %d (already in store by event_id)\n", r.Deduped)
	fmt.Fprintf(&b, "invalid:      %d (bad JSON or failed envelope validation)\n", r.Invalid)
	fmt.Fprintf(&b, "duration_ms:  %d", r.DurationMS)
	return b.String()
}

// ImportJSONL reads envelopes line-by-line from r and inserts each
// into the store. Envelopes are batched into chunked transactions
// (see envelopeBatcher) to amortise SQLite's per-commit fsync cost;
// a malformed envelope inside a chunk falls back to per-row replay
// so one bad line never aborts the surrounding rows.
// ctx is propagated to every store write so Ctrl-C stops an import
// between chunks rather than after the current file.
//
// Idempotent via event_id PK; rerunning on the same input is safe.
func ImportJSONL(ctx context.Context, r io.Reader, s *store.Store) (ImportReport, error) {
	start := time.Now()
	report := ImportReport{}
	batcher := newEnvelopeBatcher(s)

	sc := bufio.NewScanner(r)
	// Envelopes can carry long assistant messages — widen the default
	// 64KB token cap to something that handles realistic turns.
	sc.Buffer(make([]byte, 1<<20), 16<<20)

	for sc.Scan() {
		report.LinesRead++
		line := sc.Bytes()
		if len(bytesTrimSpace(line)) == 0 {
			continue
		}

		var env ingest.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			report.Invalid++
			continue
		}
		if err := env.Validate(); err != nil {
			report.Invalid++
			continue
		}

		// Scrub every envelope on import. The input file may be a
		// pre-redaction export, a third-party JSONL dump, or a buggy
		// client — we don't trust Redaction.Applied on the wire.
		// After scrubbing we re-marshal so the bytes we persist match
		// the scrubbed in-memory envelope.
		ingest.ApplyRedaction(&env, redact.Default())
		scrubbed, err := json.Marshal(&env)
		if err != nil {
			report.Invalid++
			continue
		}

		// Need a stable copy of env per envelope: Add stashes a pointer
		// and the loop variable would otherwise be reused for the next
		// iteration's fresh Unmarshal target.
		envCopy := env
		if err := batcher.Add(ctx, &envCopy, scrubbed); err != nil {
			report.DurationMS = time.Since(start).Milliseconds()
			report.Imported = batcher.Imported()
			report.Deduped = batcher.Deduped()
			return report, fmt.Errorf("import line %d (%s): %w", report.LinesRead, env.EventID, err)
		}
	}
	if err := sc.Err(); err != nil {
		report.DurationMS = time.Since(start).Milliseconds()
		report.Imported = batcher.Imported()
		report.Deduped = batcher.Deduped()
		return report, fmt.Errorf("scan: %w", err)
	}

	if err := batcher.Flush(ctx); err != nil {
		report.DurationMS = time.Since(start).Milliseconds()
		report.Imported = batcher.Imported()
		report.Deduped = batcher.Deduped()
		return report, fmt.Errorf("flush: %w", err)
	}

	report.Imported = batcher.Imported()
	report.Deduped = batcher.Deduped()
	report.DurationMS = time.Since(start).Milliseconds()
	return report, nil
}

// bytesTrimSpace avoids the strings.TrimSpace copy for empty-line
// detection on a hot loop.
func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isASCIISpace(b[start]) {
		start++
	}
	for end > start && isASCIISpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
