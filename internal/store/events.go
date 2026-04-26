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

// SessionCompletion is one row returned by LoadSessionsForCompletion.
// ID is the full session UUID (callers passing 8-char prefixes still
// get the canonical form back); Description is a single human-
// readable line composed of cwd + first user_prompt preview, suitable
// for shells that surface descriptions next to each candidate (zsh,
// fish).
type SessionCompletion struct {
	ID          string
	Description string
}

const sessionCompletionPreviewMax = 80

// LoadSessionsForCompletion returns up to limit sessions whose id
// starts with prefix, newest-first, with a one-line description.
// Designed for cobra completion functions: opens a read-only path,
// expects to be called interactively, must return fast.
//
// The query uses the same idx_sessions_effective_ts expression
// index as the MCP list_sessions tool, so a blank prefix is just
// "the most recent N sessions" without a sort. Prefix is validated
// before reaching SQL so LIKE wildcards can't slip in.
func LoadSessionsForCompletion(ctx context.Context, db *sql.DB, prefix string, limit int) ([]SessionCompletion, error) {
	if limit <= 0 {
		limit = 50
	}
	prefix = strings.ToLower(prefix)
	for _, r := range prefix {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex && r != '-' {
			// Anything non-hex means the user is mid-typing
			// something else; silently return empty rather than
			// erroring — completion funcs shouldn't yell.
			return nil, nil
		}
	}

	rows, err := db.QueryContext(ctx,
		`SELECT s.id,
		        COALESCE(s.cwd, '-') AS cwd,
		        COALESCE(
		            (SELECT content_text FROM events
		               WHERE session_id = s.id AND kind = 'user_prompt'
		               ORDER BY ts_source_ms ASC LIMIT 1),
		            '-'
		        ) AS first_prompt
		   FROM sessions s
		  WHERE s.id LIKE ? || '%'
		  ORDER BY COALESCE(s.ended_at_ms, s.started_at_ms, 0) DESC
		  LIMIT ?`,
		prefix, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query session completions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionCompletion
	for rows.Next() {
		var id, cwd, firstPrompt string
		if err := rows.Scan(&id, &cwd, &firstPrompt); err != nil {
			return nil, fmt.Errorf("scan completion row: %w", err)
		}
		out = append(out, SessionCompletion{
			ID:          id,
			Description: formatCompletionDescription(cwd, firstPrompt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completion rows: %w", err)
	}
	return out, nil
}

// formatCompletionDescription packs cwd and the first-prompt preview
// into one line. Newlines and tabs flatten because shell completion
// frameworks treat tab as the field separator and break on a literal
// newline. Long previews truncate so a shell column doesn't blow out.
func formatCompletionDescription(cwd, firstPrompt string) string {
	preview := firstPrompt
	for _, r := range "\n\r\t" {
		preview = strings.ReplaceAll(preview, string(r), " ")
	}
	runes := []rune(preview)
	if len(runes) > sessionCompletionPreviewMax {
		preview = string(runes[:sessionCompletionPreviewMax]) + "…"
	}
	return cwd + " — " + preview
}

// SubagentExists reports whether any event has the given
// subagent_id. Used by the MCP search_events handler to give the
// caller a clean error when they pass an unknown subagent_id
// rather than silently returning zero hits — which is
// indistinguishable from "real subagent with no matches" and
// hides typos from the agent.
func SubagentExists(ctx context.Context, db *sql.DB, subagentID string) (bool, error) {
	if subagentID == "" {
		return false, nil
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE subagent_id = ? LIMIT 1`,
		subagentID,
	).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("subagent existence check: %w", err)
	}
	return true, nil
}

// SubagentSpan is one row from LoadSubagentSpans: a sub-agent
// thread aggregated from the subagent_id-bearing events that
// share a (session_id, subagent_id) pair.
type SubagentSpan struct {
	SessionID    string
	SubagentID   string
	SubagentType sql.NullString
	StartedAtMs  int64
	EndedAtMs    int64
	EventCount   int
}

// LoadSubagentSpans returns aggregated sub-agent threads, newest
// (last-event) first. When sessionID is non-empty, narrows to
// threads in that session; otherwise returns spans across the
// whole store.
//
// Backed by the partial index idx_events_subagent introduced in
// migration 007. The aggregate is cheap because the index covers
// (session_id, subagent_id, ts_source_ms) and only top-level
// events (subagent_id IS NULL) are skipped.
func LoadSubagentSpans(ctx context.Context, db *sql.DB, sessionID string, limit int) ([]SubagentSpan, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		q    string
		args []any
	)
	// GROUP BY only the identity keys (session_id, subagent_id).
	// subagent_type is descriptive metadata and a host CAN report
	// inconsistent types for the same thread (e.g. "planner"
	// during start, "research-planner" mid-flight). Including
	// type in GROUP BY would fragment one logical thread into
	// multiple rows; MAX() picks one deterministic value
	// (lexicographically last non-null) without fragmenting.
	if sessionID == "" {
		q = `SELECT session_id, subagent_id, MAX(subagent_type) AS subagent_type,
		            MIN(ts_source_ms) AS started_at_ms,
		            MAX(ts_source_ms) AS ended_at_ms,
		            COUNT(*) AS event_count
		       FROM events
		      WHERE subagent_id IS NOT NULL
		      GROUP BY session_id, subagent_id
		      ORDER BY ended_at_ms DESC
		      LIMIT ?`
		args = []any{limit}
	} else {
		q = `SELECT session_id, subagent_id, MAX(subagent_type) AS subagent_type,
		            MIN(ts_source_ms) AS started_at_ms,
		            MAX(ts_source_ms) AS ended_at_ms,
		            COUNT(*) AS event_count
		       FROM events
		      WHERE subagent_id IS NOT NULL AND session_id = ?
		      GROUP BY session_id, subagent_id
		      ORDER BY ended_at_ms DESC
		      LIMIT ?`
		args = []any{sessionID, limit}
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query subagent spans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SubagentSpan
	for rows.Next() {
		var s SubagentSpan
		if err := rows.Scan(&s.SessionID, &s.SubagentID, &s.SubagentType,
			&s.StartedAtMs, &s.EndedAtMs, &s.EventCount); err != nil {
			return nil, fmt.Errorf("scan subagent span: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate subagent spans: %w", err)
	}
	return out, nil
}

// EventView is the read-only shape used by code that walks stored
// events (prompt builders, export, audit). Nullable fields are
// modelled with sql.Null* so callers can distinguish "empty string"
// from "column was NULL".
type EventView struct {
	EventID      string
	Kind         string
	Role         sql.NullString
	ContentText  sql.NullString
	TsSourceMs   int64
	ToolName     sql.NullString
	SubagentID   sql.NullString
	SubagentType sql.NullString
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
		`SELECT event_id, kind, role, content_text, ts_source_ms, tool_name,
		        subagent_id, subagent_type
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
		if err := rows.Scan(&e.EventID, &e.Kind, &e.Role, &e.ContentText,
			&e.TsSourceMs, &e.ToolName, &e.SubagentID, &e.SubagentType); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
