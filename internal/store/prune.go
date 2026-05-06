package store

import (
	"context"
	"database/sql"
	"fmt"
)

// PruneReport summarises a prune run. Counts are populated for
// both dry-run and live runs so the operator gets the same shape
// either way; live runs reflect actual deletes, dry-runs reflect
// what would have been deleted.
type PruneReport struct {
	Sessions     int   `json:"sessions"`
	RawEnvelopes int   `json:"raw_envelopes"`
	Events       int   `json:"events"`
	Extractions  int   `json:"extractions"`
	LLMOutputs   int   `json:"llm_outputs"` // only non-zero if IncludeLLMOutputs was set
	DryRun       bool  `json:"dry_run"`
	CutoffMs     int64 `json:"cutoff_ms"`
}

// PruneOptions drives Prune.
type PruneOptions struct {
	// CutoffMs is the lower bound: rows whose ended_at_ms is
	// strictly less than this are pruned. Sessions with ended_at
	// NULL (still active) are protected.
	CutoffMs int64

	// IncludeLLMOutputs, when true, also deletes every llm_outputs
	// row whose created_at_ms < cutoff. Default behaviour
	// preserves them — summaries / reflections are expensive to
	// regenerate. The session-scoped ones survive as orphans
	// (session_id becomes NULL via the schema's ON DELETE SET NULL).
	IncludeLLMOutputs bool

	// DryRun reports the would-be counts without mutating the
	// store. Default false; the CLI flips it on by default and
	// requires --yes to actually delete.
	DryRun bool
}

// Prune deletes sessions older than CutoffMs and everything they
// own (raw_envelopes / events / extractions / events_fts), in a
// single transaction so a partial failure rolls back cleanly.
//
// Active sessions (ended_at_ms IS NULL) are protected, regardless
// of how old started_at_ms is — the assumption is that the user
// started something months ago and never closed it; we don't
// want to lose that on a sweep.
//
// The schema cascades from raw_envelopes(event_id) → events,
// from events(session_id) → sessions, and from extractions →
// events; events_fts is kept in sync via the
// events_fts_ad / events_fts_au triggers in 001_initial.sql. So a
// single DELETE on raw_envelopes covers events / extractions / fts;
// a separate DELETE on sessions removes the (now orphan-of-events)
// session rows.
//
// llm_outputs.session_id is ON DELETE SET NULL so deleting a
// session preserves its summary as a historical record. Pass
// IncludeLLMOutputs to also drop those records on this pass.
func Prune(ctx context.Context, db *sql.DB, opts PruneOptions) (PruneReport, error) {
	report := PruneReport{
		CutoffMs: opts.CutoffMs,
		DryRun:   opts.DryRun,
	}

	// Snapshot the would-be deletions in a read-only pass so
	// dry-runs land identical numbers to live runs and so the
	// final report can show them all even after the cascades
	// have erased the source rows.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE ended_at_ms IS NOT NULL AND ended_at_ms < ?`,
		opts.CutoffMs,
	).Scan(&report.Sessions); err != nil {
		return report, fmt.Errorf("count sessions: %w", err)
	}
	// raw_envelopes/events/extractions live behind the session
	// cutoff transitively; count via a JOIN so the report
	// reflects what cascades will eat.
	for table, dest := range map[string]*int{
		"raw_envelopes": &report.RawEnvelopes,
		"events":        &report.Events,
		"extractions":   &report.Extractions,
	} {
		var q string
		switch table {
		case "raw_envelopes":
			q = `SELECT COUNT(*) FROM raw_envelopes r
			      WHERE r.event_id IN (
			        SELECT e.event_id FROM events e
			          JOIN sessions s ON s.id = e.session_id
			         WHERE s.ended_at_ms IS NOT NULL AND s.ended_at_ms < ?
			      )`
		case "events":
			q = `SELECT COUNT(*) FROM events e
			      JOIN sessions s ON s.id = e.session_id
			     WHERE s.ended_at_ms IS NOT NULL AND s.ended_at_ms < ?`
		case "extractions":
			q = `SELECT COUNT(*) FROM extractions x
			      JOIN sessions s ON s.id = x.session_id
			     WHERE s.ended_at_ms IS NOT NULL AND s.ended_at_ms < ?`
		}
		if err := db.QueryRowContext(ctx, q, opts.CutoffMs).Scan(dest); err != nil {
			return report, fmt.Errorf("count %s: %w", table, err)
		}
	}
	if opts.IncludeLLMOutputs {
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM llm_outputs WHERE created_at_ms < ?`,
			opts.CutoffMs,
		).Scan(&report.LLMOutputs); err != nil {
			return report, fmt.Errorf("count llm_outputs: %w", err)
		}
	}

	if opts.DryRun {
		return report, nil
	}

	// Live delete: single transaction so a failure midway leaves
	// the DB consistent. Order matters — raw_envelopes first so
	// the cascade chains through events/extractions/fts, sessions
	// second to drop the now-empty session rows.
	if err := WithTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM raw_envelopes
			  WHERE event_id IN (
			    SELECT e.event_id FROM events e
			      JOIN sessions s ON s.id = e.session_id
			     WHERE s.ended_at_ms IS NOT NULL AND s.ended_at_ms < ?
			  )`,
			opts.CutoffMs,
		); err != nil {
			return fmt.Errorf("delete raw_envelopes: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE ended_at_ms IS NOT NULL AND ended_at_ms < ?`,
			opts.CutoffMs,
		); err != nil {
			return fmt.Errorf("delete sessions: %w", err)
		}
		if opts.IncludeLLMOutputs {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM llm_outputs WHERE created_at_ms < ?`,
				opts.CutoffMs,
			); err != nil {
				return fmt.Errorf("delete llm_outputs: %w", err)
			}
		}
		return nil
	}); err != nil {
		return report, err
	}
	return report, nil
}

// Vacuum reclaims space released by a prune. It runs
//
//	PRAGMA wal_checkpoint(TRUNCATE)
//	VACUUM
//
// in that order: the checkpoint flushes any pending WAL frames
// into the main DB so VACUUM sees the latest state and can shrink
// the freelist; truncating the WAL after lets the OS reclaim it.
//
// VACUUM rewrites the entire database into a temp file and
// renames it on completion, so it requires (a) the only writer
// (other connections in WAL mode tolerate it as readers, but a
// concurrent writer will block) and (b) up to ~2× the DB size in
// free disk during the run. The CLI prints both warnings.
func Vacuum(ctx context.Context, db *sql.DB) error {
	// Checkpoint truncates the WAL after merging frames into the
	// main file. Truncate (not full / passive) is the variant
	// that asks SQLite to shrink the WAL back to zero bytes.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("wal_checkpoint: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

// PageInfo is the page-count snapshot used by tests and the CLI
// vacuum report to show before/after numbers.
type PageInfo struct {
	PageCount int64
	PageSize  int64
}

// Bytes returns the on-disk size implied by the page metadata.
func (i PageInfo) Bytes() int64 { return i.PageCount * i.PageSize }

// QueryPageInfo runs the two PRAGMAs SQLite exposes for sizing.
// Cheap; safe to call before/after vacuum.
func QueryPageInfo(ctx context.Context, db *sql.DB) (PageInfo, error) {
	var info PageInfo
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&info.PageCount); err != nil {
		return info, fmt.Errorf("page_count: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&info.PageSize); err != nil {
		return info, fmt.Errorf("page_size: %w", err)
	}
	return info, nil
}
