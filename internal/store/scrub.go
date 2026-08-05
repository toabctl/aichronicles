package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
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
	EventsScanned       int `json:"events_scanned"`
	EventsRewritten     int `json:"events_rewritten"`
	EnvelopesRewritten  int `json:"envelopes_rewritten"`
	LLMOutputsScanned   int `json:"llm_outputs_scanned"`
	LLMOutputsRewritten int `json:"llm_outputs_rewritten"`

	// ExtractionsScanned/Rewritten cover extractions.value and
	// extractions.extra_json. Extractions are projected out of event
	// content at ingest, so a secret in an event body is copied here
	// too — and extractions_fts makes it searchable, which is how a
	// "successfully scrubbed" secret stayed greppable via
	// `aichronicles search`.
	ExtractionsScanned   int `json:"extractions_scanned"`
	ExtractionsRewritten int `json:"extractions_rewritten"`

	// SessionsRederived counts sessions whose materialised columns
	// (cwd, start_cwd, first_prompt_text, summary_topic) were
	// recomputed because one of their source rows was rewritten.
	SessionsRederived int `json:"sessions_rederived"`

	PatternHits map[string]int `json:"pattern_hits"`
	DryRun      bool           `json:"dry_run"`
}

// Scrub walks every secret-bearing column, applies the scanner, and
// rewrites matches to <redacted:kind> markers. Coverage:
//
//   - raw_envelopes.envelope_json, plus its events projection
//     (content_text AND cwd — ApplyRedaction scrubs both)
//   - extractions.value / extra_json, which copy event content at
//     ingest and are indexed by extractions_fts
//   - llm_outputs.body, which never passes through the edge redactor
//   - the materialised sessions columns, re-derived afterwards
//     because their triggers are AFTER INSERT only
//
// Anything less is a false sense of safety: an operator runs this
// after adding a detector, sees "rewrote N", and reasonably concludes
// the secret is gone. It has to actually be gone everywhere.
//
// The FTS indexes need no explicit maintenance — events_fts,
// events_fts_trigram and extractions_fts all have AFTER UPDATE
// triggers that mirror column rewrites.
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

	// Sessions whose rows were rewritten. The materialised columns on
	// `sessions` are fed by AFTER INSERT triggers only, so an UPDATE
	// to the underlying events/llm_outputs never propagates; we
	// re-derive them explicitly for exactly the sessions we touched.
	affected := map[string]struct{}{}

	if err := scrubRawEnvelopes(ctx, tx, scanner, opts, report, out, affected); err != nil {
		return report, err
	}
	if err := scrubExtractions(ctx, tx, scanner, opts, report, out, affected); err != nil {
		return report, err
	}
	if err := scrubLLMOutputs(ctx, tx, scanner, opts, report, out, affected); err != nil {
		return report, err
	}
	if !opts.DryRun {
		if err := rederiveSessionColumns(ctx, tx, affected); err != nil {
			return report, err
		}
	}
	report.SessionsRederived = len(affected)

	summary := fmt.Sprintf(
		"scanned=%d envelopes_rewritten=%d events_content_rewritten=%d "+
			"extractions_scanned=%d extractions_rewritten=%d "+
			"llm_outputs_scanned=%d llm_outputs_rewritten=%d dry_run=%t\n",
		report.EventsScanned, report.EnvelopesRewritten, report.EventsRewritten,
		report.ExtractionsScanned, report.ExtractionsRewritten,
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

func scrubRawEnvelopes(ctx context.Context, tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer, affected map[string]struct{}) error {
	// LEFT JOIN so an envelope with no projected event row (a
	// malformed ingest) still gets its raw JSON scrubbed.
	rows, err := tx.QueryContext(ctx, `SELECT r.event_id, r.envelope_json, e.session_id
		  FROM raw_envelopes r
		  LEFT JOIN events e ON e.event_id = r.event_id
		 ORDER BY r.ingest_seq ASC`)
	if err != nil {
		return fmt.Errorf("list raw_envelopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rewrite struct {
		eventID      string
		newRaw       string
		newContent   sql.NullString
		contentDirty bool
		newCwd       sql.NullString
		cwdDirty     bool
		patterns     []string
	}
	var pending []rewrite

	for rows.Next() {
		report.EventsScanned++
		var eventID, rawJSON string
		var sessionID sql.NullString
		if err := rows.Scan(&eventID, &rawJSON, &sessionID); err != nil {
			return fmt.Errorf("scan raw row: %w", err)
		}

		var env events.Envelope
		if err := json.Unmarshal([]byte(rawJSON), &env); err != nil {
			_, _ = fmt.Fprintf(out, "skip %s: malformed envelope_json: %v\n", scrubFirstN(eventID, 8), err)
			continue
		}
		originalContent := env.ContentText
		originalCwd := env.Cwd
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
			// ApplyRedaction scrubs Cwd as well as ContentText — paths
			// legitimately carry secrets (a temp dir named after a
			// token). Projecting only content_text left the scrubbed
			// value in the envelope but the raw one in events.cwd.
			cwdDirty: env.Cwd != originalCwd,
			patterns: env.Redaction.Patterns,
		}
		if rw.contentDirty {
			rw.newContent = sql.NullString{String: env.ContentText, Valid: env.ContentText != ""}
		}
		if rw.cwdDirty {
			rw.newCwd = sql.NullString{String: env.Cwd, Valid: env.Cwd != ""}
		}
		if sessionID.Valid {
			affected[sessionID.String] = struct{}{}
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
			if rw.cwdDirty {
				if _, err := tx.ExecContext(ctx,
					`UPDATE events SET cwd = ? WHERE event_id = ?`,
					rw.newCwd, rw.eventID,
				); err != nil {
					return fmt.Errorf("update events cwd %s: %w", rw.eventID, err)
				}
			}
		}
		_, _ = fmt.Fprintf(out, "%s %s patterns=%v\n", mode, scrubFirstN(rw.eventID, 8), rw.patterns)
	}
	return nil
}

// scrubExtractions rewrites extractions.value and extractions.extra_json.
//
// Extractions are projections of event content produced at ingest, so
// a secret in a tool_use payload is copied here verbatim — a
// shell_command extraction of `curl -H "Authorization: Bearer <tok>"`
// holds the token in full. Skipping this table meant Scrub reported
// success while `aichronicles search <secret>` still returned the row,
// because extractions_fts indexes value.
//
// The extractions_fts_au trigger (migration 006) keeps the FTS index
// in sync on UPDATE, so rewriting the column is sufficient — no
// explicit index maintenance here.
func scrubExtractions(ctx context.Context, tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer, affected map[string]struct{}) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, session_id, value, extra_json FROM extractions ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("list extractions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rewrite struct {
		id       int64
		newValue string
		newExtra sql.NullString
		patterns []string
	}
	var pending []rewrite

	for rows.Next() {
		report.ExtractionsScanned++
		var id int64
		var sessionID, extra sql.NullString
		var value string
		if err := rows.Scan(&id, &sessionID, &value, &extra); err != nil {
			return fmt.Errorf("scan extractions row: %w", err)
		}

		// redact.Replace already returns a unique, sorted name list per
		// column; the set just unions the two columns' lists.
		fired := map[string]struct{}{}
		cleanValue, valueNames := redact.Replace(value, scanner.Scan(value))
		cleanExtra := extra.String
		var extraNames []string
		if extra.Valid {
			cleanExtra, extraNames = redact.Replace(extra.String, scanner.Scan(extra.String))
		}
		if cleanValue == value && cleanExtra == extra.String {
			continue
		}
		for _, n := range valueNames {
			fired[n] = struct{}{}
		}
		for _, n := range extraNames {
			fired[n] = struct{}{}
		}

		if sessionID.Valid {
			affected[sessionID.String] = struct{}{}
		}
		pending = append(pending, rewrite{
			id:       id,
			newValue: cleanValue,
			newExtra: sql.NullString{String: cleanExtra, Valid: extra.Valid},
			patterns: scrubSortedKeys(fired),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()

	for _, rw := range pending {
		for _, p := range rw.patterns {
			report.PatternHits[p]++
		}
		report.ExtractionsRewritten++

		mode := "would rewrite"
		if !opts.DryRun {
			mode = "rewrote"
			if _, err := tx.ExecContext(ctx,
				`UPDATE extractions SET value = ?, extra_json = ? WHERE id = ?`,
				rw.newValue, rw.newExtra, rw.id,
			); err != nil {
				return fmt.Errorf("update extractions %d: %w", rw.id, err)
			}
		}
		_, _ = fmt.Fprintf(out, "%s extraction id=%d patterns=%v\n", mode, rw.id, rw.patterns)
	}
	return nil
}

func scrubLLMOutputs(ctx context.Context, tx *sql.Tx, scanner redact.Scanner, opts ScrubOptions, report *ScrubReport, out io.Writer, affected map[string]struct{}) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, body FROM llm_outputs ORDER BY id ASC`)
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
		var sessionID sql.NullString
		var body string
		if err := rows.Scan(&id, &sessionID, &body); err != nil {
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
		if sessionID.Valid {
			affected[sessionID.String] = struct{}{}
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

// rederiveSessionColumns recomputes the four materialised columns on
// `sessions` for the given session ids.
//
// Migrations 001/015/016 keep these current with AFTER INSERT
// triggers. There is no AFTER UPDATE counterpart, so rewriting
// events.content_text / events.cwd / llm_outputs.body leaves the
// projections holding the pre-scrub text. That is how a scrubbed
// secret stayed visible in the sessions list, in shell completion
// and in the MCP project-context tool.
//
// Each expression below is the canonical one from the migration that
// introduced the column, so a re-derivation cannot drift from what a
// fresh backfill would produce:
//
//   - cwd:               last non-null cwd in INSERT order (rowid),
//     matching sessions_agg_ai's COALESCE(new.cwd, cwd) accumulation
//     rather than timestamp order, which would differ for
//     out-of-order imports.
//   - start_cwd:         first non-null cwd in event-time order.
//   - first_prompt_text: earliest user_prompt with non-null content.
//   - summary_topic:     $.topic of the newest kind=summary body.
//
// Scoped to affected sessions on purpose: recomputing every session
// would silently "fix" rows the scrub never touched, and any
// disagreement between a trigger and its re-derivation would then
// rewrite correct data. Narrow beats broad here.
func rederiveSessionColumns(ctx context.Context, tx *sql.Tx, affected map[string]struct{}) error {
	if len(affected) == 0 {
		return nil
	}
	const stmt = `
		UPDATE sessions SET
			cwd = (
				SELECT e.cwd FROM events e
				 WHERE e.session_id = sessions.id AND e.cwd IS NOT NULL
				 ORDER BY e.rowid DESC LIMIT 1
			),
			start_cwd = (
				SELECT e.cwd FROM events e
				 WHERE e.session_id = sessions.id AND e.cwd IS NOT NULL
				 ORDER BY e.ts_source_ms ASC, e.rowid ASC LIMIT 1
			),
			first_prompt_text = (
				SELECT e.content_text FROM events e
				 WHERE e.session_id = sessions.id
				   AND e.kind = 'user_prompt'
				   AND e.content_text IS NOT NULL
				 ORDER BY e.ts_source_ms ASC, e.rowid ASC LIMIT 1
			),
			summary_topic = (
				SELECT CASE WHEN json_valid(l.body)
				            THEN json_extract(l.body, '$.topic')
				            ELSE NULL END
				  FROM llm_outputs l
				 WHERE l.session_id = sessions.id AND l.kind = 'summary'
				 ORDER BY l.created_at_ms DESC LIMIT 1
			)
		WHERE id = ?`

	for id := range affected {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("re-derive session columns for %s: %w", id, err)
		}
	}
	return nil
}

// scrubSortedKeys returns a map's keys in sorted order. Generic over
// the value type so the same helper serves both the PatternHits
// counter map and the per-row pattern sets, rather than each caller
// growing its own copy.
func scrubSortedKeys[V any](m map[string]V) []string {
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
