package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
)

func newScrubCmd() *cobra.Command {
	var (
		dryRun bool
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
			resolved := dbPath
			if resolved == "" {
				p, err := paths.StorePath()
				if err != nil {
					return err
				}
				resolved = p
			}
			s, err := store.Open(resolved)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			opts := ScrubOptions{DryRun: dryRun || !yes}
			if !opts.DryRun && !yes {
				return errors.New("scrub: refusing to write without --yes")
			}
			_, err = RunScrub(s, redact.Default(), opts, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report changes without writing (default: on unless --yes)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm irreversible writes (required to mutate the DB)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// ScrubOptions controls scrub-time behavior. DryRun=true means "scan
// and report, do not write" — the default unless --yes is passed.
type ScrubOptions struct {
	DryRun bool
}

// ScrubReport summarizes what scrub did (or would do under --dry-run).
type ScrubReport struct {
	EventsScanned     int
	EventsRewritten   int
	RawBytesRewritten int
	PatternHits       map[string]int
	DryRun            bool
}

// RunScrub walks every raw envelope, applies the scanner to it, and
// rewrites both raw_envelopes.envelope_json and events.content_text
// when changes are needed. Each rewrite is its own transaction so a
// mid-run failure leaves the DB consistent — some events scrubbed,
// the rest unchanged.
//
// Returns the report even on error so callers see progress so far.
func RunScrub(s *store.Store, scanner redact.Scanner, opts ScrubOptions, out io.Writer) (*ScrubReport, error) {
	report := &ScrubReport{PatternHits: map[string]int{}, DryRun: opts.DryRun}

	rows, err := s.DB().Query(`SELECT event_id, envelope_json FROM raw_envelopes ORDER BY ingest_seq ASC`)
	if err != nil {
		return report, fmt.Errorf("list raw_envelopes: %w", err)
	}

	type rewrite struct {
		eventID      string
		newRaw       string
		newContent   sql.NullString
		contentDirty bool
		patterns     []string
	}
	var pending []rewrite

	for rows.Next() {
		report.EventsScanned++
		var eventID, rawJSON string
		if err := rows.Scan(&eventID, &rawJSON); err != nil {
			_ = rows.Close()
			return report, fmt.Errorf("scan raw row: %w", err)
		}

		var env ingest.Envelope
		if err := json.Unmarshal([]byte(rawJSON), &env); err != nil {
			_, _ = fmt.Fprintf(out, "skip %s: malformed envelope_json: %v\n", firstN(eventID, 8), err)
			continue
		}
		originalContent := env.ContentText
		ingest.ApplyRedaction(&env, scanner)

		if len(env.Redaction.Patterns) == 0 {
			continue
		}

		reMarshaled, err := json.Marshal(&env)
		if err != nil {
			_ = rows.Close()
			return report, fmt.Errorf("re-marshal %s: %w", eventID, err)
		}

		rw := rewrite{
			eventID:      eventID,
			newRaw:       string(reMarshaled),
			contentDirty: env.ContentText != originalContent,
			patterns:     env.Redaction.Patterns,
		}
		if rw.contentDirty {
			rw.newContent = sql.NullString{String: env.ContentText, Valid: env.ContentText != ""}
		}
		pending = append(pending, rw)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return report, err
	}
	_ = rows.Close()

	for _, rw := range pending {
		for _, p := range rw.patterns {
			report.PatternHits[p]++
		}
		report.RawBytesRewritten++
		if rw.contentDirty {
			report.EventsRewritten++
		}

		mode := "would rewrite"
		if !opts.DryRun {
			mode = "rewrote"
			if err := applyScrubWrite(s.DB(), rw.eventID, rw.newRaw, rw.newContent, rw.contentDirty); err != nil {
				return report, fmt.Errorf("write %s: %w", rw.eventID, err)
			}
		}
		_, _ = fmt.Fprintf(out, "%s %s patterns=%v\n", mode, firstN(rw.eventID, 8), rw.patterns)
	}

	summary := fmt.Sprintf("scanned=%d raw_rewritten=%d events_content_rewritten=%d dry_run=%t\n",
		report.EventsScanned, report.RawBytesRewritten, report.EventsRewritten, report.DryRun)
	_, _ = fmt.Fprint(out, summary)
	for _, p := range sortedKeys(report.PatternHits) {
		_, _ = fmt.Fprintf(out, "  %-24s %d\n", p, report.PatternHits[p])
	}
	return report, nil
}

// applyScrubWrite does the two UPDATEs in one transaction so either
// both succeed or neither does. Updating events.content_text fires the
// events_fts_au trigger, keeping FTS consistent.
func applyScrubWrite(db *sql.DB, eventID, newRaw string, newContent sql.NullString, contentDirty bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE raw_envelopes SET envelope_json = ? WHERE event_id = ?`,
		newRaw, eventID,
	); err != nil {
		return fmt.Errorf("update raw_envelopes: %w", err)
	}
	if contentDirty {
		if _, err := tx.Exec(
			`UPDATE events SET content_text = ? WHERE event_id = ?`,
			newContent, eventID,
		); err != nil {
			return fmt.Errorf("update events: %w", err)
		}
	}
	return tx.Commit()
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
