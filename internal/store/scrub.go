package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/pkg/events"
)

// ScrubOptions controls Scrub behavior. DryRun=true means "scan
// and report, do not write" — the safe default when invoked
// from a CLI / api admin path.
type ScrubOptions struct {
	DryRun bool

	// Out, when non-nil, receives one line per scanned/rewritten
	// row so an interactive caller can stream progress. Nil sends
	// progress to io.Discard. Either way the final ScrubReport
	// carries the same totals, so a non-streaming caller (e.g.
	// the api admin endpoint) just passes nil.
	Out io.Writer
}

// ScrubReport summarizes what Scrub did (or would do under
// DryRun). Counts are row counts, not byte counts.
type ScrubReport struct {
	EventsScanned       int            `json:"events_scanned"`
	EventsRewritten     int            `json:"events_rewritten"`
	EnvelopesRewritten  int            `json:"envelopes_rewritten"`
	LLMOutputsScanned   int            `json:"llm_outputs_scanned"`
	LLMOutputsRewritten int            `json:"llm_outputs_rewritten"`
	PatternHits         map[string]int `json:"pattern_hits"`
	DryRun              bool           `json:"dry_run"`
}

// Scrub walks every raw envelope and every cached LLM output,
// applies the scanner, and rewrites matches to <redacted:kind>
// markers in both write paths to the store: raw_envelopes (with
// the events projection) and llm_outputs (LLM responses that
// never go through ingest's edge redactor).
//
// The whole run is one transaction. Either the entire detector-
// set upgrade lands or nothing does — no half-scrubbed state on
// a crash. The cost is holding SQLite's write lock for the
// duration of the scan; readers (web, MCP, CLI search) keep
// working via WAL, but other writers (the daemon, imports)
// block. That's acceptable for a maintenance operation the user
// explicitly opted into.
//
// Returns the report even on error so callers see progress so far.
func Scrub(ctx context.Context, db *sql.DB, scanner redact.Scanner, opts ScrubOptions) (*ScrubReport, error) {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	report := &ScrubReport{PatternHits: map[string]int{}, DryRun: opts.DryRun}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin scrub tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := scrubRawEnvelopes(ctx, tx, scanner, opts, report, out); err != nil {
		return report, err
	}
	if err := scrubLLMOutputs(ctx, tx, scanner, opts, report, out); err != nil {
		return report, err
	}

	summary := fmt.Sprintf(
		"scanned=%d envelopes_rewritten=%d events_content_rewritten=%d "+
			"llm_outputs_scanned=%d llm_outputs_rewritten=%d dry_run=%t\n",
		report.EventsScanned, report.EnvelopesRewritten, report.EventsRewritten,
		report.LLMOutputsScanned, report.LLMOutputsRewritten, report.DryRun)
	_, _ = fmt.Fprint(out, summary)
	for _, p := range scrubSortedKeys(report.PatternHits) {
		_, _ = fmt.Fprintf(out, "  %-24s %d\n", p, report.PatternHits[p])
	}

	if !opts.DryRun {
		if err := tx.Commit(); err != nil {
			return report, fmt.Errorf("commit scrub: %w", err)
		}
	}
	return report, nil
}

func scrubRawEnvelopes(ctx context.Context, tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer) error {
	rows, err := tx.QueryContext(ctx, `SELECT event_id, envelope_json FROM raw_envelopes ORDER BY ingest_seq ASC`)
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

		var env events.Envelope
		if err := json.Unmarshal([]byte(rawJSON), &env); err != nil {
			_, _ = fmt.Fprintf(out, "skip %s: malformed envelope_json: %v\n", scrubFirstN(eventID, 8), err)
			continue
		}
		originalContent := env.ContentText
		events.ApplyRedaction(&env, scanner)

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
			if _, err := tx.ExecContext(ctx,
				`UPDATE raw_envelopes SET envelope_json = ? WHERE event_id = ?`,
				rw.newRaw, rw.eventID,
			); err != nil {
				return fmt.Errorf("update raw_envelopes %s: %w", rw.eventID, err)
			}
			if rw.contentDirty {
				if _, err := tx.ExecContext(ctx,
					`UPDATE events SET content_text = ? WHERE event_id = ?`,
					rw.newContent, rw.eventID,
				); err != nil {
					return fmt.Errorf("update events %s: %w", rw.eventID, err)
				}
			}
		}
		_, _ = fmt.Fprintf(out, "%s %s patterns=%v\n", mode, scrubFirstN(rw.eventID, 8), rw.patterns)
	}
	return nil
}

func scrubLLMOutputs(ctx context.Context, tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, body FROM llm_outputs ORDER BY id ASC`)
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
			if _, err := tx.ExecContext(ctx,
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

func scrubSortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scrubFirstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
