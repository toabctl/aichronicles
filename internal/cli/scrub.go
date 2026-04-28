package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/redact"
)

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

			_, err = RunScrub(s, redact.Default(), ScrubOptions{DryRun: !yes}, cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm irreversible writes (required to mutate the DB)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

// ScrubOptions controls scrub-time behavior. DryRun=true means "scan
// and report, do not write" — the default unless --yes is passed.
type ScrubOptions struct {
	DryRun bool
}

// ScrubReport summarizes what scrub did (or would do under --dry-run).
// Counts are row counts, not byte counts — one envelope with a
// 40-char key contributes 1 to EnvelopesRewritten, not 40.
type ScrubReport struct {
	EventsScanned       int
	EventsRewritten     int // events.content_text rewrites (implies raw was rewritten too)
	EnvelopesRewritten  int // raw_envelopes.envelope_json rewrites; >= EventsRewritten
	LLMOutputsScanned   int
	LLMOutputsRewritten int // llm_outputs.body rewrites
	PatternHits         map[string]int
	DryRun              bool
}

// RunScrub walks every raw envelope and every cached LLM output,
// applies the scanner, and rewrites matches to <redacted:kind>
// markers in both write paths to the store: raw_envelopes (with
// the events projection) and llm_outputs (LLM responses that
// never go through ingest's edge redactor).
//
// The whole run is one transaction. Either the entire detector-set
// upgrade lands or nothing does — no half-scrubbed state on a
// crash. The cost is holding SQLite's write lock for the duration
// of the scan; readers (web, MCP, CLI search) keep working via WAL,
// but other writers (the daemon, imports) block. That's acceptable
// for a maintenance operation the user explicitly opted into.
//
// Returns the report even on error so callers see progress so far.
func RunScrub(s *store.Store, scanner redact.Scanner, opts ScrubOptions, out io.Writer) (*ScrubReport, error) {
	report := &ScrubReport{PatternHits: map[string]int{}, DryRun: opts.DryRun}

	tx, err := s.DB().Begin()
	if err != nil {
		return report, fmt.Errorf("begin scrub tx: %w", err)
	}
	// Always rollback on the way out; commit (when not dry-run)
	// happens explicitly at the end of the success path.
	defer func() { _ = tx.Rollback() }()

	if err := scrubRawEnvelopes(tx, scanner, opts, report, out); err != nil {
		return report, err
	}
	if err := scrubLLMOutputs(tx, scanner, opts, report, out); err != nil {
		return report, err
	}

	summary := fmt.Sprintf(
		"scanned=%d envelopes_rewritten=%d events_content_rewritten=%d "+
			"llm_outputs_scanned=%d llm_outputs_rewritten=%d dry_run=%t\n",
		report.EventsScanned, report.EnvelopesRewritten, report.EventsRewritten,
		report.LLMOutputsScanned, report.LLMOutputsRewritten, report.DryRun)
	_, _ = fmt.Fprint(out, summary)
	for _, p := range sortedKeys(report.PatternHits) {
		_, _ = fmt.Fprintf(out, "  %-24s %d\n", p, report.PatternHits[p])
	}

	if !opts.DryRun {
		if err := tx.Commit(); err != nil {
			return report, fmt.Errorf("commit scrub: %w", err)
		}
	}
	return report, nil
}

// scrubRawEnvelopes walks raw_envelopes inside the open transaction,
// rewriting envelope_json and the corresponding events.content_text
// in place when the scanner finds anything.
func scrubRawEnvelopes(tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer) error {
	rows, err := tx.Query(`SELECT event_id, envelope_json FROM raw_envelopes ORDER BY ingest_seq ASC`)
	if err != nil {
		return fmt.Errorf("list raw_envelopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

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
			return fmt.Errorf("scan raw row: %w", err)
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
			return fmt.Errorf("re-marshal %s: %w", eventID, err)
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
		return err
	}
	_ = rows.Close()

	for _, rw := range pending {
		for _, p := range rw.patterns {
			report.PatternHits[p]++
		}
		report.EnvelopesRewritten++
		if rw.contentDirty {
			report.EventsRewritten++
		}

		mode := "would rewrite"
		if !opts.DryRun {
			mode = "rewrote"
			if _, err := tx.Exec(
				`UPDATE raw_envelopes SET envelope_json = ? WHERE event_id = ?`,
				rw.newRaw, rw.eventID,
			); err != nil {
				return fmt.Errorf("update raw_envelopes %s: %w", rw.eventID, err)
			}
			if rw.contentDirty {
				if _, err := tx.Exec(
					`UPDATE events SET content_text = ? WHERE event_id = ?`,
					rw.newContent, rw.eventID,
				); err != nil {
					return fmt.Errorf("update events %s: %w", rw.eventID, err)
				}
			}
		}
		_, _ = fmt.Fprintf(out, "%s %s patterns=%v\n", mode, firstN(rw.eventID, 8), rw.patterns)
	}
	return nil
}

// scrubLLMOutputs walks llm_outputs and rewrites bodies that match
// the current detector set. LLM outputs land in the store via
// SaveLLMOutput, not via /v1/ingest — so they bypass the edge
// redactor entirely. Without this loop, an upgraded detector set
// catches new patterns in events but stale bodies in llm_outputs
// keep emitting the old leak through every read path.
func scrubLLMOutputs(tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer) error {
	rows, err := tx.Query(`SELECT id, body FROM llm_outputs ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("list llm_outputs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rewrite struct {
		id       int64
		newBody  string
		patterns []string
	}
	var pending []rewrite

	for rows.Next() {
		report.LLMOutputsScanned++
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return fmt.Errorf("scan llm_outputs row: %w", err)
		}
		findings := scanner.Scan(body)
		if len(findings) == 0 {
			continue
		}
		clean, names := redact.Replace(body, findings)
		if clean == body {
			continue
		}
		pending = append(pending, rewrite{id: id, newBody: clean, patterns: names})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()

	for _, rw := range pending {
		for _, p := range rw.patterns {
			report.PatternHits[p]++
		}
		report.LLMOutputsRewritten++

		mode := "would rewrite"
		if !opts.DryRun {
			mode = "rewrote"
			if _, err := tx.Exec(
				`UPDATE llm_outputs SET body = ? WHERE id = ?`,
				rw.newBody, rw.id,
			); err != nil {
				return fmt.Errorf("update llm_outputs %d: %w", rw.id, err)
			}
		}
		_, _ = fmt.Fprintf(out, "%s llm_output id=%d patterns=%v\n", mode, rw.id, rw.patterns)
	}
	return nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
