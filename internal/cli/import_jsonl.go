package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
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
			"historical events into SQLite, or to replay a backup.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedDB := dbPath
			if resolvedDB == "" {
				p, err := paths.StorePath()
				if err != nil {
					return err
				}
				resolvedDB = p
			}
			s, err := store.Open(resolvedDB)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("open %s: %w", args[0], err)
			}
			defer func() { _ = f.Close() }()

			report, err := ImportJSONL(f, s)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
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
// into the store. Every line runs in its own transaction — that way a
// mid-file malformed line doesn't abort earlier successful writes.
//
// Idempotent via event_id PK; rerunning on the same input is safe.
func ImportJSONL(r io.Reader, s *store.Store) (ImportReport, error) {
	start := time.Now()
	report := ImportReport{}

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

		deduped, err := importOne(s, &env, line)
		if err != nil {
			// Storage-level error is fatal — something is wrong with
			// the DB, not the input.
			report.DurationMS = time.Since(start).Milliseconds()
			return report, fmt.Errorf("import line %d (%s): %w", report.LinesRead, env.EventID, err)
		}
		if deduped {
			report.Deduped++
		} else {
			report.Imported++
		}
	}
	if err := sc.Err(); err != nil {
		report.DurationMS = time.Since(start).Milliseconds()
		return report, fmt.Errorf("scan: %w", err)
	}

	report.DurationMS = time.Since(start).Milliseconds()
	return report, nil
}

// importOne wraps one envelope insertion in its own transaction.
func importOne(s *store.Store, env *ingest.Envelope, raw []byte) (bool, error) {
	tx, err := s.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	tsServer := time.Now().UTC().UnixMilli()
	deduped, err := store.IngestEnvelope(tx, env, raw, tsServer)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return deduped, nil
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
