package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNoSuchSession is returned when a session-id prefix does not match
// any row. Callers typically wrap this with a feature-specific message.
var ErrNoSuchSession = errors.New("no such session")

// ErrAmbiguousSessionPrefix is returned when a prefix matches more than
// one session. The error string embeds up to ambiguityListLimit ids so
// the user can disambiguate without shelling into sqlite.
var ErrAmbiguousSessionPrefix = errors.New("ambiguous session prefix")

// ambiguityListLimit caps how many candidates we list on an ambiguous
// prefix. Enough to recognise which one you meant, not so many that a
// "0" prefix spams the terminal.
const ambiguityListLimit = 5

// ResolveSessionIDPrefix resolves a user-supplied session identifier —
// typically the 8-char prefix `aichronicles sessions` prints — to the
// single full session id in the store. A full UUID also works: the
// LIKE match is trivially satisfied.
//
// Returns ErrNoSuchSession if no row matches, or
// ErrAmbiguousSessionPrefix (wrapped with up to ambiguityListLimit
// matching ids) if the prefix is under-specified. The input must be
// lowercase hex + hyphens to keep SQLite's LIKE wildcards out of the
// query — session ids are UUID strings so this is always true for
// legitimate input.
func ResolveSessionIDPrefix(ctx context.Context, db *sql.DB, prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("session id is required")
	}
	prefix = strings.ToLower(prefix)
	for _, r := range prefix {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex && r != '-' {
			return "", fmt.Errorf("session id must be hex or hyphens, got %q", prefix)
		}
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id FROM sessions WHERE id LIKE ? || '%' LIMIT ?`,
		prefix, ambiguityListLimit+1,
	)
	if err != nil {
		return "", fmt.Errorf("resolve session prefix: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("scan session id: %w", err)
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate sessions: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", ErrNoSuchSession, prefix)
	case 1:
		return matches[0], nil
	default:
		// More than one — show up to ambiguityListLimit so the user
		// can pick, and hint there might be more.
		shown := matches
		hint := ""
		if len(shown) > ambiguityListLimit {
			shown = shown[:ambiguityListLimit]
			hint = " (…)"
		}
		return "", fmt.Errorf("%w %q matches %d sessions: %s%s",
			ErrAmbiguousSessionPrefix, prefix, len(matches),
			strings.Join(shown, ", "), hint)
	}
}

// EventView is the read-only shape used by code that walks stored
// events (prompt builders, export, audit). Nullable fields are
// modelled with sql.Null* so callers can distinguish "empty string"
// from "column was NULL".
type EventView struct {
	EventID     string
	Kind        string
	Role        sql.NullString
	ContentText sql.NullString
	TsSourceMs  int64
	ToolName    sql.NullString
}

// SessionDigestRow is the read shape used by reflect/propose. Each
// row represents one conversation: its time window, the first user
// prompt (so the model has something to anchor on when there is no
// summary yet), and the latest llm_outputs summary if any.
//
// Callers fold this into the prompts.SessionDigest payload; keeping
// the DB-facing shape separate lets us evolve either side without
// coupling.
type SessionDigestRow struct {
	ID            string
	StartedAtMs   sql.NullInt64
	EndedAtMs     sql.NullInt64
	Cwd           sql.NullString
	FirstPrompt   sql.NullString
	LatestSummary sql.NullString
}

// LoadRecentSessionDigests returns the most-recently-ended sessions
// whose ended_at is within the `sinceMs` cutoff, newest first,
// capped at `limit`. Sessions with a NULL ended_at fall back to
// started_at so mid-flight captures aren't invisible.
//
// Both subqueries (first_prompt, latest_summary) are correlated;
// at thousand-session scale the cost is negligible. If that ever
// becomes the slow spot, materialize them into columns.
func LoadRecentSessionDigests(ctx context.Context, db *sql.DB, sinceMs int64, limit int) ([]SessionDigestRow, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := db.QueryContext(ctx,
		`SELECT s.id, s.started_at_ms, s.ended_at_ms, s.cwd,
			(SELECT content_text FROM events
				WHERE session_id = s.id AND kind = 'user_prompt'
				ORDER BY ts_source_ms ASC LIMIT 1) AS first_prompt,
			(SELECT body FROM llm_outputs
				WHERE session_id = s.id AND kind = 'summary'
				ORDER BY created_at_ms DESC LIMIT 1) AS latest_summary
		FROM sessions s
		WHERE COALESCE(s.ended_at_ms, s.started_at_ms, 0) >= ?
		ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
		LIMIT ?`,
		sinceMs, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query session digests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionDigestRow
	for rows.Next() {
		var r SessionDigestRow
		if err := rows.Scan(&r.ID, &r.StartedAtMs, &r.EndedAtMs, &r.Cwd, &r.FirstPrompt, &r.LatestSummary); err != nil {
			return nil, fmt.Errorf("scan digest: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DefaultEventsPerSessionLimit caps LoadEventsForSession's result set
// when the caller does not supply a tighter bound. 10k events per
// session is generous (an hour of hot tool use is typically under 1k
// events) and keeps a pathological session from loading 1M rows into
// memory for a summarize call.
const DefaultEventsPerSessionLimit = 10_000

// Extraction is one row from the extractions table — a typed fact
// pulled out of an event at ingest time (URL, file path, shell
// command, etc.). Callers usually want them deduped by value, which
// is what LoadExtractionsForSession does.
type Extraction struct {
	Kind  string
	Value string
}

// LoadExtractionsForSession returns every distinct extraction of the
// given kind for a session, ordered by first-appearance timestamp so
// downstream callers see them in the order the user produced them.
// Dedup is on value — the same URL mentioned 10 times across a
// session yields one row.
//
// Empty kind is rejected: callers should target one kind per query
// (kind='url', kind='file_path', …) so the return type stays simple.
func LoadExtractionsForSession(ctx context.Context, db *sql.DB, sessionID, kind string) ([]Extraction, error) {
	if kind == "" {
		return nil, errors.New("LoadExtractionsForSession: kind is required")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT x.kind, x.value
		 FROM extractions x
		 WHERE x.session_id = ? AND x.kind = ?
		 GROUP BY x.value
		 ORDER BY MIN(x.id) ASC`,
		sessionID, kind,
	)
	if err != nil {
		return nil, fmt.Errorf("query extractions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Extraction
	for rows.Next() {
		var e Extraction
		if err := rows.Scan(&e.Kind, &e.Value); err != nil {
			return nil, fmt.Errorf("scan extraction: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LoadEventsForSession returns up to `limit` events for a session,
// oldest first. An empty slice is returned for an unknown session.
// A non-positive `limit` uses DefaultEventsPerSessionLimit — callers
// that truly want "every event" should pass a very large number and
// own the memory consequences.
func LoadEventsForSession(ctx context.Context, db *sql.DB, sessionID string, limit int) ([]EventView, error) {
	if limit <= 0 {
		limit = DefaultEventsPerSessionLimit
	}
	rows, err := db.QueryContext(ctx,
		`SELECT event_id, kind, role, content_text, ts_source_ms, tool_name
		 FROM events
		 WHERE session_id = ?
		 ORDER BY ts_source_ms ASC, rowid ASC
		 LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventView
	for rows.Next() {
		var e EventView
		if err := rows.Scan(&e.EventID, &e.Kind, &e.Role, &e.ContentText, &e.TsSourceMs, &e.ToolName); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
