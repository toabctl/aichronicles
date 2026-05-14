package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/toabctl/aichronicles/internal/wire"
)

// SessionLink* constants live in internal/wire — they're protocol
// vocabulary the summarize prompt emits and the web/CLI both
// consume. These re-exports keep existing store.SessionLinkX
// callers working unchanged; new code should use wire.SessionLinkX
// directly. arch_review_2026_05_13 LOW followup.
const (
	SessionLinkBuildsOn         = wire.SessionLinkBuildsOn
	SessionLinkRepeatsFailureOf = wire.SessionLinkRepeatsFailureOf
	SessionLinkSupersedes       = wire.SessionLinkSupersedes
	SessionLinkRelated          = wire.SessionLinkRelated
)

// SessionLinkKinds is the canonical ordered list, re-exported from
// internal/wire so existing callers keep working.
var SessionLinkKinds = wire.SessionLinkKinds

// IsValidSessionLinkKind matches the migration's CHECK clause —
// keep these in sync if a fifth kind is ever added.
func IsValidSessionLinkKind(k string) bool {
	return slices.Contains(SessionLinkKinds, k)
}

// SessionLink is one (from, to, kind) row.
type SessionLink struct {
	FromSessionID string
	ToSessionID   string
	Kind          string
	Rationale     string
	CreatedAtMs   int64
}

// CandidateSession is a row returned by LoadCandidatePriorSessions:
// a recent session in the same cwd that summarize can show the LLM
// as a possible link target. Topic comes from the latest persisted
// summary (empty string if the candidate hasn't been summarized).
type CandidateSession struct {
	ID          string
	Cwd         string
	StartedAtMs int64
	EndedAtMs   int64
	Topic       string
}

// LoadCandidatePriorSessions returns up to `limit` sessions in the
// same cwd as `forSession` that ended before `forSession` started.
// Same-cwd is the cheap-but-right anchor: it captures "I was working
// on the same project a week ago" without needing semantic similarity
// over event embeddings (a future upgrade — see migration 009).
//
// Returned rows are newest-first (most recent ended_at). Sessions
// with no cwd or with cwd != forSession.cwd are skipped. The query
// joins llm_outputs to surface the latest summary topic without a
// second round-trip.
func LoadCandidatePriorSessions(ctx context.Context, db *sql.DB, forSession string, limit int) ([]CandidateSession, error) {
	if limit <= 0 {
		limit = 10
	}

	// Get the anchor session's cwd + started_at. If either is
	// missing we can't anchor candidates — return empty rather
	// than fall back to a global "recent sessions" query, which
	// would dilute the link signal with unrelated work.
	var cwd sql.NullString
	var startedAt sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT cwd, started_at_ms FROM sessions WHERE id = ?`, forSession,
	).Scan(&cwd, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load anchor session: %w", err)
	}
	if cwd.String == "" || !startedAt.Valid {
		return nil, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT
		  s.id,
		  s.cwd,
		  COALESCE(s.started_at_ms, 0),
		  COALESCE(s.ended_at_ms, 0),
		  COALESCE(s.summary_topic, '') AS topic
		FROM sessions s
		WHERE s.cwd = ?
		  AND s.id != ?
		  AND `+EffectiveTsExpr+` < ?
		ORDER BY `+EffectiveTsExpr+` DESC
		LIMIT ?
	`, cwd.String, forSession, startedAt.Int64, limit)
	if err != nil {
		return nil, fmt.Errorf("load candidate sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CandidateSession
	for rows.Next() {
		var c CandidateSession
		if err := rows.Scan(&c.ID, &c.Cwd, &c.StartedAtMs, &c.EndedAtMs, &c.Topic); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return out, nil
}

// SaveSessionLinks replaces every link emitted from `from` with the
// given set, atomically. Re-summarizing a session shouldn't leave
// stale links from an earlier run, so we DELETE-then-INSERT inside
// a transaction. Empty `links` clears all outgoing links.
//
// Each link's Kind must be in SessionLinkKinds; the migration's
// CHECK clause backstops this but we validate up-front for a
// clearer error than "constraint failed".
func SaveSessionLinks(ctx context.Context, db *sql.DB, from string, links []SessionLink) error {
	for i, l := range links {
		if l.ToSessionID == "" {
			return fmt.Errorf("link[%d]: to_session_id is empty", i)
		}
		if l.ToSessionID == from {
			return fmt.Errorf("link[%d]: self-link (to == from)", i)
		}
		if !IsValidSessionLinkKind(l.Kind) {
			return fmt.Errorf("link[%d]: invalid kind %q (allowed: %v)", i, l.Kind, SessionLinkKinds)
		}
	}

	return WithTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM session_links WHERE from_session_id = ?`, from); err != nil {
			return fmt.Errorf("clear existing links: %w", err)
		}
		now := time.Now().UnixMilli()
		for _, l := range links {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO session_links(from_session_id, to_session_id, kind, rationale, created_at_ms)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(from_session_id, to_session_id, kind) DO UPDATE SET
				  rationale     = excluded.rationale,
				  created_at_ms = excluded.created_at_ms
			`, from, l.ToSessionID, l.Kind, l.Rationale, now)
			if err != nil {
				return fmt.Errorf("insert link to %s (%s): %w", l.ToSessionID, l.Kind, err)
			}
		}
		return nil
	})
}

// LoadSessionLinksFrom returns every link emitted from `sessionID`,
// ordered by kind (canonical order) then to_session_id. Empty slice
// when there are no links — never returns nil for "no rows".
func LoadSessionLinksFrom(ctx context.Context, db *sql.DB, sessionID string) ([]SessionLink, error) {
	return loadSessionLinks(ctx, db, `
		SELECT from_session_id, to_session_id, kind, COALESCE(rationale, ''), created_at_ms
		  FROM session_links
		 WHERE from_session_id = ?
		 ORDER BY
		   CASE kind
		     WHEN 'builds_on'          THEN 1
		     WHEN 'repeats_failure_of' THEN 2
		     WHEN 'supersedes'         THEN 3
		     WHEN 'related'            THEN 4
		     ELSE 5
		   END,
		   to_session_id
	`, sessionID)
}

// LoadSessionLinksTo returns every link pointing AT `sessionID`,
// from any other session — the reverse-index half of the sidebar.
// Same ordering as LoadSessionLinksFrom.
func LoadSessionLinksTo(ctx context.Context, db *sql.DB, sessionID string) ([]SessionLink, error) {
	return loadSessionLinks(ctx, db, `
		SELECT from_session_id, to_session_id, kind, COALESCE(rationale, ''), created_at_ms
		  FROM session_links
		 WHERE to_session_id = ?
		 ORDER BY
		   CASE kind
		     WHEN 'builds_on'          THEN 1
		     WHEN 'repeats_failure_of' THEN 2
		     WHEN 'supersedes'         THEN 3
		     WHEN 'related'            THEN 4
		     ELSE 5
		   END,
		   from_session_id
	`, sessionID)
}

func loadSessionLinks(ctx context.Context, db *sql.DB, query, sessionID string) ([]SessionLink, error) {
	rows, err := db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SessionLink, 0)
	for rows.Next() {
		var l SessionLink
		if err := rows.Scan(&l.FromSessionID, &l.ToSessionID, &l.Kind, &l.Rationale, &l.CreatedAtMs); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}
