package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

// backfillBatchSize caps how many envelopes we re-process in one
// transaction. Each batch holds a write lock on SQLite; smaller =
// less contention with the live ingest path; larger = fewer
// fsyncs. 500 is the same balance import-claude's batcher uses.
const backfillBatchSize = 500

func newBackfillExtractionsCmd() *cobra.Command {
	var (
		dbPath   string
		sockPath string
		only     string
	)
	cmd := &cobra.Command{
		Use:   "backfill-extractions",
		Short: "Re-run extractors over every raw envelope and rewrite the extractions table",
		Long: "Walks every row in raw_envelopes, deserialises envelope_json,\n" +
			"and rewrites the extractions table to match what the current\n" +
			"set of extractors would produce. Use this when a new extractor\n" +
			"lands (skill_load, web_query, …) and you want it applied to\n" +
			"events ingested before it existed — without wiping the store\n" +
			"and re-importing.\n\n" +
			"Idempotent. With --only=<kind>, only rows matching that kind\n" +
			"are deleted/replaced; other kinds are left untouched.\n" +
			"Without --only, ALL extraction rows are rebuilt from scratch.\n\n" +
			"Refuses to run while aichronicles-api is up: this command\n" +
			"rewrites the daemon-owned extractions table and racing the\n" +
			"IngestWorker would leave inconsistent rows. Stop the\n" +
			"daemon first (systemctl --user stop aichronicles-api),\n" +
			"run this, then restart.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := RefuseIfDaemonRunning(cmd.Context(), sockPath); err != nil {
				return err
			}
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(),
				&slog.HandlerOptions{Level: slog.LevelInfo})).
				With("cmd", "aichronicles backfill-extractions")
			report, err := RunBackfillExtractions(cmd.Context(), s, only, log)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().StringVar(&sockPath, "socket", "",
		"aichronicles-api UDS path used to check whether the daemon is running (overrides $AICHRONICLES_API_SOCKET; XDG default)")
	cmd.Flags().StringVar(&only, "only", "",
		"only rebuild this extraction kind (e.g. skill_load); empty = all kinds")
	return cmd
}

// BackfillReport summarises what RunBackfillExtractions did.
type BackfillReport struct {
	EnvelopesScanned int
	EnvelopesParsed  int            // successfully unmarshalled
	Invalid          int            // envelope_json that didn't parse — skipped
	Deleted          int            // extraction rows DELETEd
	Inserted         int            // extraction rows INSERTed
	ByKind           map[string]int // inserted per kind
	DurationMS       int64
}

func (r BackfillReport) String() string {
	out := fmt.Sprintf("envelopes scanned:   %d\n", r.EnvelopesScanned)
	out += fmt.Sprintf("envelopes parsed:    %d\n", r.EnvelopesParsed)
	out += fmt.Sprintf("invalid:             %d\n", r.Invalid)
	out += fmt.Sprintf("rows deleted:        %d\n", r.Deleted)
	out += fmt.Sprintf("rows inserted:       %d\n", r.Inserted)
	out += "inserted by kind:\n"
	// Stable order so the report diffs cleanly across runs.
	kinds := make([]string, 0, len(r.ByKind))
	for k := range r.ByKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		out += fmt.Sprintf("  %-20s %d\n", k, r.ByKind[k])
	}
	out += fmt.Sprintf("duration_ms:         %d", r.DurationMS)
	return out
}

// RunBackfillExtractions walks raw_envelopes in ingest_seq order
// and rewrites the extractions table. With onlyKind="", ALL rows
// are rebuilt; with onlyKind=<name>, only rows of that kind are
// touched and inserts are filtered to that kind.
func RunBackfillExtractions(ctx context.Context, s *store.Store, onlyKind string, log *slog.Logger) (BackfillReport, error) {
	start := time.Now()
	report := BackfillReport{ByKind: map[string]int{}}

	total, err := countRawEnvelopes(ctx, s.DB())
	if err != nil {
		return report, fmt.Errorf("count: %w", err)
	}
	if total == 0 {
		report.DurationMS = time.Since(start).Milliseconds()
		log.Info("backfill: no envelopes")
		return report, nil
	}
	if onlyKind != "" {
		log.Info("backfill starting", "envelopes", total, "only_kind", onlyKind)
	} else {
		log.Info("backfill starting", "envelopes", total)
	}

	var lastSeq int64 = -1
	for {
		batch, err := loadRawBatch(ctx, s.DB(), lastSeq, backfillBatchSize)
		if err != nil {
			return report, fmt.Errorf("load batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		if err := rewriteBatch(ctx, s.DB(), batch, onlyKind, &report); err != nil {
			return report, fmt.Errorf("rewrite batch: %w", err)
		}
		lastSeq = batch[len(batch)-1].ingestSeq
		log.Info("backfill progress",
			"scanned", report.EnvelopesScanned,
			"of_total", total,
			"inserted", report.Inserted,
		)
	}

	report.DurationMS = time.Since(start).Milliseconds()
	log.Info("backfill done",
		"scanned", report.EnvelopesScanned,
		"inserted", report.Inserted,
		"deleted", report.Deleted,
		"duration_ms", report.DurationMS,
	)
	return report, nil
}

// rawEnvelopeRow is the projection we read out of raw_envelopes for
// each batch — just enough to drive extraction. We don't need
// session_id from the row because it's also on env.SourceSessionID
// and we re-derive the canonical session_id via DeriveSessionID,
// matching how IngestEnvelope sets it on insert.
type rawEnvelopeRow struct {
	ingestSeq    int64
	eventID      string
	envelopeJSON []byte
}

func countRawEnvelopes(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_envelopes`).Scan(&n)
	return n, err
}

// loadRawBatch reads up to limit rows whose ingest_seq is greater
// than afterSeq, ordered ascending. afterSeq=-1 means "from the
// start." We page by ingest_seq because raw_envelopes is
// append-only and ingest_seq is monotonic — gives us a stable
// resumable cursor without OFFSET's quadratic cost.
func loadRawBatch(ctx context.Context, db *sql.DB, afterSeq int64, limit int) ([]rawEnvelopeRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ingest_seq, event_id, envelope_json
		   FROM raw_envelopes
		  WHERE ingest_seq > ?
		  ORDER BY ingest_seq ASC
		  LIMIT ?`,
		afterSeq, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []rawEnvelopeRow
	for rows.Next() {
		var r rawEnvelopeRow
		if err := rows.Scan(&r.ingestSeq, &r.eventID, &r.envelopeJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rewriteBatch processes one slice of envelopes inside a single
// transaction. For each envelope it (1) deletes existing
// extraction rows for the event_id (filtered by onlyKind when
// set), (2) re-extracts from the envelope, and (3) inserts the
// fresh rows. Failures on a single envelope (malformed JSON)
// don't fail the whole batch — they're counted into report.Invalid.
func rewriteBatch(ctx context.Context, db *sql.DB, batch []rawEnvelopeRow, onlyKind string, report *BackfillReport) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, row := range batch {
		report.EnvelopesScanned++
		var env events.Envelope
		if err := json.Unmarshal(row.envelopeJSON, &env); err != nil {
			report.Invalid++
			continue
		}
		report.EnvelopesParsed++

		sessionID := events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)

		var del sql.Result
		if onlyKind != "" {
			del, err = tx.ExecContext(ctx,
				`DELETE FROM extractions WHERE event_id = ? AND kind = ?`,
				row.eventID, onlyKind,
			)
		} else {
			del, err = tx.ExecContext(ctx,
				`DELETE FROM extractions WHERE event_id = ?`,
				row.eventID,
			)
		}
		if err != nil {
			return fmt.Errorf("delete extractions for %s: %w", row.eventID, err)
		}
		if n, _ := del.RowsAffected(); n > 0 {
			report.Deleted += int(n)
		}

		for _, ex := range events.DefaultExtractors().Run(&env) {
			if onlyKind != "" && ex.Kind != onlyKind {
				continue
			}
			var extraJSON sql.NullString
			if len(ex.Extra) > 0 {
				if b, err := json.Marshal(ex.Extra); err == nil {
					extraJSON = sql.NullString{String: string(b), Valid: true}
				}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO extractions(event_id, session_id, kind, value, extra_json) VALUES (?, ?, ?, ?, ?)`,
				row.eventID, sessionID, ex.Kind, ex.Value, extraJSON,
			); err != nil {
				return fmt.Errorf("insert extraction (%s=%q): %w", ex.Kind, ex.Value, err)
			}
			report.Inserted++
			report.ByKind[ex.Kind]++
		}
	}

	return tx.Commit()
}

// Compile-time guard: keep the io.Writer-based logger constructor
// import alive even if other tests stop using it.
var _ = (io.Writer)(nil)
