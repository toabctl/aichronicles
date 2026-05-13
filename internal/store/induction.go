package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultInductionIdle is the canonical "session has ended" idle
// threshold: no new events for this long means we count the
// session as finished. The CLI's --idle flag, the daemon's
// Induction.Idle config setting, and LoadInductionCandidates'
// idleThresholdMs <= 0 fallback all consume this single value so
// the three paths can't drift.
const DefaultInductionIdle = 30 * time.Minute

// InductionCandidate is one session ripe for online induction:
// idle long enough that we believe it's ended (no new events for
// >= idleThresholdMs), substantial enough to be worth a model
// call (>= minEvents), and not yet processed (no llm_outputs row
// of kind=induction).
//
// Nullable timestamps and cwd use sql.Null* so the shape matches
// SessionDigestRow / TopSession; callers see "not set" instead of
// a fabricated zero. Only ID and EventCount are guaranteed non-null
// by the WHERE clause (sessions.ended_at_ms IS NOT NULL +
// event_count >= minEvents > 0).
type InductionCandidate struct {
	ID          string
	StartedAtMs sql.NullInt64
	EndedAtMs   sql.NullInt64
	Cwd         sql.NullString
	EventCount  int
}

// LoadInductionCandidates returns up to `limit` sessions that
// satisfy all four predicates:
//
//  1. Idle: ended_at_ms <= nowMs - idleThresholdMs (so we don't
//     induce a session that's still actively receiving events).
//
//  2. Substantial: event_count >= minEvents (drops the trivial
//     "user typed `q`" sessions that won't ground a useful skill).
//
//  3. Not already processed: no llm_outputs row exists with
//     kind=induction AND session_id = s.id. Re-running the
//     sweeper is then idempotent at the per-session level —
//     once we've induced (or decided no_skill_found), we don't
//     pay the LLM call again.
//
//  4. No pending envelopes: no ingest_pending row belongs to
//     this session. The async ingest path (cmd/aichronicles-api +
//     internal/api/ingest_worker) can leave already-accepted
//     envelopes queued in ingest_pending after the trigger-
//     maintained sessions.ended_at_ms aggregate has already
//     advanced. Without this predicate, a session whose latest
//     events are still pending would look idle to the sweeper
//     and the LLM would summarise a truncated transcript. The
//     source_agent / source_session_id pair is the natural join
//     key (DeriveSessionID is a deterministic UUIDv5 of those
//     two fields), so we extract them from the queued JSON
//     envelope rather than denormalising into ingest_pending.
//
// Newest-ended first, so an interactive sweep surfaces the most
// recently completed work before catching up on backlog.
//
// Concurrency note: this function does NOT serialise concurrent
// callers. The daemon's InductionSweeper and a manual
// `aichronicles induction sweep` can run at the same time; both
// see the same candidate set, both kick off LLM calls for
// overlapping sessions. SaveLLMOutput is idempotent on
// (kind, prompt_hash) so the second write coalesces into the
// first — no corruption — but the LLM is paid for twice during
// the overlap. Acceptable: the sweeper is opt-in, the manual
// command is interactive, and the cost is bounded by limit per
// run. If this ever bites, hold an advisory lock on the seq
// table for the duration of one sweep.
//
// idleThresholdMs <= 0 falls back to DefaultInductionIdle.
// minEvents <= 0 falls back to 5 (a one-prompt session almost
// never grounds a workflow). limit <= 0 falls back to 50.
func LoadInductionCandidates(ctx context.Context, db *sql.DB, nowMs, idleThresholdMs int64, minEvents, limit int) ([]InductionCandidate, error) {
	if idleThresholdMs <= 0 {
		idleThresholdMs = DefaultInductionIdle.Milliseconds()
	}
	if minEvents <= 0 {
		minEvents = 5
	}
	if limit <= 0 {
		limit = 50
	}
	cutoff := nowMs - idleThresholdMs

	q := `
		SELECT s.id,
		       s.started_at_ms,
		       s.ended_at_ms,
		       s.cwd,
		       s.event_count
		  FROM sessions s
		 WHERE s.ended_at_ms IS NOT NULL
		   AND s.ended_at_ms <= ?
		   AND s.event_count >= ?
		   AND NOT EXISTS (
		     SELECT 1 FROM llm_outputs lo
		      WHERE lo.session_id = s.id AND lo.kind = ?
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM ingest_pending ip
		      WHERE json_extract(ip.body, '$.source_agent') = s.source_agent
		        AND json_extract(ip.body, '$.source_session_id') = s.source_session_id
		   )
		 ORDER BY s.ended_at_ms DESC
		 LIMIT ?
	`
	rows, err := db.QueryContext(ctx, q, cutoff, minEvents, string(LLMKindInduction), limit)
	if err != nil {
		return nil, fmt.Errorf("query induction candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []InductionCandidate
	for rows.Next() {
		var c InductionCandidate
		if err := rows.Scan(&c.ID, &c.StartedAtMs, &c.EndedAtMs, &c.Cwd, &c.EventCount); err != nil {
			return nil, fmt.Errorf("scan induction candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasInductionRun returns true when an llm_outputs row of
// kind=induction exists for sessionID. Cheap idempotency guard
// for callers that want to skip a candidate without running the
// full LoadInductionCandidates query.
func HasInductionRun(ctx context.Context, db *sql.DB, sessionID string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM llm_outputs WHERE session_id = ? AND kind = ? LIMIT 1`,
		sessionID, string(LLMKindInduction),
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check induction: %w", err)
	}
	return true, nil
}

// LoadInductionRow returns the most recent llm_outputs row of
// kind=induction for sessionID, or nil if none exists. Used by
// the CLI to render "what did induction decide for this session?"
// without a separate body parse step.
func LoadInductionRow(ctx context.Context, db *sql.DB, sessionID string) (*LLMOutput, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, session_id, kind, model, prompt_hash,
		        input_tokens, output_tokens, body, created_at_ms
		   FROM llm_outputs
		  WHERE session_id = ? AND kind = ?
		  ORDER BY created_at_ms DESC
		  LIMIT 1`,
		sessionID, string(LLMKindInduction),
	)
	out, err := scanLLMOutput(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load induction row: %w", err)
	}
	return out, nil
}
