package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/internal/preview"
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
		        s.first_prompt_text AS first_prompt,
		        (SELECT body FROM llm_outputs
		           WHERE session_id = s.id AND kind = ?
		           ORDER BY created_at_ms DESC LIMIT 1) AS summary_body
		   FROM sessions s
		  WHERE s.id LIKE ? || '%'
		  ORDER BY `+EffectiveTsExpr+` DESC
		  LIMIT ?`,
		string(LLMKindSummary), prefix, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query session completions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionCompletion
	for rows.Next() {
		var (
			id, cwd     string
			firstPrompt sql.NullString
			summaryBody sql.NullString
		)
		if err := rows.Scan(&id, &cwd, &firstPrompt, &summaryBody); err != nil {
			return nil, fmt.Errorf("scan completion row: %w", err)
		}
		out = append(out, SessionCompletion{
			ID:          id,
			Description: formatCompletionDescription(cwd, firstPrompt.String, summaryBody.String),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completion rows: %w", err)
	}
	return out, nil
}

// formatCompletionDescription packs cwd and a session preview into
// one line for shell completion. Preview priority:
//
//  1. The cached summary's `topic` field (the model's distillation
//     of what the session was about — the strongest signal).
//  2. The first user_prompt, but only when ≥30 chars after trim and
//     not a slash command. Skips fillers like "go ahead" / "/loop"
//     that misrepresent the session.
//  3. A muted "(no summary)" placeholder, so completion is honest
//     about the lack of a topic instead of surfacing a confusing
//     fragment.
//
// Newlines and tabs flatten because shell completion frameworks
// treat tab as the field separator and break on a literal newline.
// Long previews truncate so the candidate fits one terminal line.
func formatCompletionDescription(cwd, firstPrompt, summaryBody string) string {
	preview := pickCompletionPreview(firstPrompt, summaryBody)
	for _, r := range "\n\r\t" {
		preview = strings.ReplaceAll(preview, string(r), " ")
	}
	runes := []rune(preview)
	if len(runes) > sessionCompletionPreviewMax {
		preview = string(runes[:sessionCompletionPreviewMax]) + "…"
	}
	return cwd + " — " + preview
}

// pickCompletionPreview implements the same priority order the web
// sessions list uses — keeping the helper local so the store doesn't
// depend on web, but delegating the choice to internal/preview so
// the rules can't drift. The summary body is JSON; we extract the
// `topic` field with a minimal scan rather than pulling in the
// whole prompts package.
func pickCompletionPreview(firstPrompt, summaryBody string) string {
	topic := extractSummaryTopic(summaryBody)
	text, kind := preview.Pick(topic, firstPrompt)
	if kind == preview.KindMuted {
		// CLI-completion empty state is shorter than the web
		// "(no summary yet)" — keep it minimal so the shell row
		// stays one line.
		return "(no summary)"
	}
	return text
}

// extractSummaryTopic pulls the `"topic": "..."` field out of a
// cached summary body without depending on the prompts package.
// Returns "" when the body is empty, isn't JSON, or has no topic.
// Loose parse is fine: a malformed summary body just falls
// through to first_prompt.
func extractSummaryTopic(body string) string {
	if body == "" {
		return ""
	}
	var parsed struct {
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Topic)
}

// LiveEvent is the read-only shape returned by LoadEventsSinceSeq.
// Carries enough fields to render a live-feed row without an extra
// query: ingest_seq is the cursor for resumption, the rest mirror
// what the sessions list renders. Snippet is rune-truncated server-
// side to keep the SSE payload small.
type LiveEvent struct {
	IngestSeq  int64
	EventID    string
	SessionID  string
	Kind       string
	TsSourceMs int64
	TsServerMs int64
	Cwd        sql.NullString
	Snippet    sql.NullString
}

// LoadEventsSinceSeq returns events whose ingest_seq is strictly
// greater than `cursor`, oldest-first, capped at limit. Used by the
// SSE /stream handler to fetch one batch per poll. Optional
// sessionID filter narrows to events in one session — empty means
// "all sessions". The cursor is ingest_seq (monotonic by design)
// not ts_server_ms (subject to clock skew).
func LoadEventsSinceSeq(ctx context.Context, db *sql.DB, cursor int64, sessionID string, limit int) ([]LiveEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		q    string
		args []any
	)
	const (
		baseSelect = `SELECT r.ingest_seq, e.event_id, e.session_id, e.kind,
		                     e.ts_source_ms, r.ts_server_ms, e.cwd, e.content_text
		                FROM raw_envelopes r
		                JOIN events e ON e.event_id = r.event_id
		               WHERE r.ingest_seq > ?`
		orderBy = ` ORDER BY r.ingest_seq ASC LIMIT ?`
	)
	if sessionID == "" {
		q = baseSelect + orderBy
		args = []any{cursor, limit}
	} else {
		q = baseSelect + ` AND e.session_id = ?` + orderBy
		args = []any{cursor, sessionID, limit}
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query live events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LiveEvent
	for rows.Next() {
		var e LiveEvent
		if err := rows.Scan(&e.IngestSeq, &e.EventID, &e.SessionID, &e.Kind,
			&e.TsSourceMs, &e.TsServerMs, &e.Cwd, &e.Snippet); err != nil {
			return nil, fmt.Errorf("scan live event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live events: %w", err)
	}
	return out, nil
}

// LoadLatestEventsIndexedByID returns the most recent event per
// session in sessionIDs, keyed by session_id. Sessions without any
// events are absent from the map (no zero-value entry, so callers
// can distinguish "no events yet" from "event with empty body").
//
// One window-function query — the alternative of calling
// LoadEventsForSession per session is N+1 and the sessions list
// renders this on every page load. ROW_NUMBER() picks the latest
// per partition; ties broken by event_id DESC (UUIDv7 event_ids
// sort by creation time, so this is deterministic).
//
// Empty input returns an empty map and no query.
func LoadLatestEventsIndexedByID(ctx context.Context, db *sql.DB, sessionIDs []string) (map[string]LiveEvent, error) {
	if len(sessionIDs) == 0 {
		return map[string]LiveEvent{}, nil
	}

	placeholders := strings.Repeat(",?", len(sessionIDs))[1:]
	args := make([]any, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		args = append(args, id)
	}

	q := `SELECT r.ingest_seq, e.event_id, e.session_id, e.kind,
		       e.ts_source_ms, r.ts_server_ms, e.cwd, e.content_text
		  FROM (
		      SELECT event_id, session_id, kind, ts_source_ms, cwd, content_text,
		             ROW_NUMBER() OVER (PARTITION BY session_id
		                                ORDER BY ts_source_ms DESC, event_id DESC) AS rn
		        FROM events
		       WHERE session_id IN (` + placeholders + `)
		  ) e
		  JOIN raw_envelopes r ON r.event_id = e.event_id
		 WHERE e.rn = 1`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query latest events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]LiveEvent, len(sessionIDs))
	for rows.Next() {
		var e LiveEvent
		if err := rows.Scan(&e.IngestSeq, &e.EventID, &e.SessionID, &e.Kind,
			&e.TsSourceMs, &e.TsServerMs, &e.Cwd, &e.Snippet); err != nil {
			return nil, fmt.Errorf("scan latest event: %w", err)
		}
		out[e.SessionID] = e
	}
	return out, rows.Err()
}

// LatestIngestSeq returns the highest ingest_seq currently in the
// store, or 0 when the store is empty. Used by the SSE /stream
// handler to seed the cursor at "now" so a new client doesn't get
// historical events flooded at it on connect.
func LatestIngestSeq(ctx context.Context, db *sql.DB) (int64, error) {
	var seq sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(ingest_seq) FROM raw_envelopes`,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("max ingest_seq: %w", err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
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
	// Cwd is the working directory recorded on the event when the
	// hook captured it. NULL for events whose hook frame omitted
	// the cwd (some hook kinds don't carry one). Populated only by
	// loaders that explicitly SELECT events.cwd; legacy paths that
	// don't need it leave the field zero-valued.
	Cwd sql.NullString
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
			s.first_prompt_text AS first_prompt,
			(SELECT body FROM llm_outputs
				WHERE session_id = s.id AND kind = ?
				ORDER BY created_at_ms DESC LIMIT 1) AS latest_summary
		FROM sessions s
		WHERE `+EffectiveTsExpr+` >= ?
		ORDER BY `+EffectiveTsExpr+` DESC
		LIMIT ?`,
		string(LLMKindSummary), sinceMs, limit,
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

// LoadSessionDigest returns the SessionDigestRow for a single
// session_id, or (nil, nil) if no such session exists. Same row
// shape as LoadRecentSessionDigests — including the latest_summary
// subquery — so the result is interchangeable.
//
// Exists to fix a class of bugs where callers loaded the recent
// list, then walked it Go-side to find one id: that misses any
// session older than the recent-list LIMIT. Single-row lookup is
// indexed (sessions.id is the primary key) and always finds the
// row when it exists.
func LoadSessionDigest(ctx context.Context, db *sql.DB, sessionID string) (*SessionDigestRow, error) {
	row := db.QueryRowContext(ctx,
		`SELECT s.id, s.started_at_ms, s.ended_at_ms, s.cwd,
			s.first_prompt_text AS first_prompt,
			(SELECT body FROM llm_outputs
				WHERE session_id = s.id AND kind = ?
				ORDER BY created_at_ms DESC LIMIT 1) AS latest_summary
		FROM sessions s
		WHERE s.id = ?`,
		string(LLMKindSummary), sessionID,
	)
	var r SessionDigestRow
	switch err := row.Scan(&r.ID, &r.StartedAtMs, &r.EndedAtMs, &r.Cwd, &r.FirstPrompt, &r.LatestSummary); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("scan session digest: %w", err)
	}
	return &r, nil
}

// SessionFilter narrows session-level queries by cwd / agent. Empty
// strings disable each filter independently; both empty == every
// session in the window. Used by LoadSessionsMissingSummary to keep
// the call shape consistent with the `sessions` CLI flags.
type SessionFilter struct {
	Cwd   string
	Agent string
}

// LoadSessionsMissingSummary returns the most-recently-ended sessions
// in the [sinceMs, ∞) window that have no llm_outputs row of
// kind='summary'. Used by `aichronicles summaries missing` and
// `aichronicles summaries fill` to enumerate the pre-LLM work
// surface — sessions that the user can summarize before running
// reflect/propose, which are mandatory-summary as of 9746cef.
//
// Same row shape as LoadRecentSessionDigests so callers can hand the
// result to the existing prompts.SessionDigest pipeline without a
// shape conversion. LatestSummary is always invalid here by
// construction (NOT EXISTS on the join) — included for shape
// compatibility, never populated.
//
// Limit ≤0 falls back to 200; a wider default than the LLM-bound
// reflect/propose because this path is read-only and just renders.
func LoadSessionsMissingSummary(ctx context.Context, db *sql.DB, sinceMs int64, filter SessionFilter, limit int) ([]SessionDigestRow, error) {
	if limit <= 0 {
		limit = 200
	}

	conds := []string{EffectiveTsExpr + " >= ?"}
	args := []any{sinceMs}
	if filter.Cwd != "" {
		conds = append(conds, "s.cwd = ?")
		args = append(args, filter.Cwd)
	}
	if filter.Agent != "" {
		conds = append(conds, "s.source_agent = ?")
		args = append(args, filter.Agent)
	}
	// The "missing" predicate: no llm_outputs row of kind=summary
	// for this session_id. NOT EXISTS rather than LEFT JOIN so the
	// optimiser can short-circuit on the first hit.
	conds = append(conds, `NOT EXISTS (
		SELECT 1 FROM llm_outputs lo
		 WHERE lo.session_id = s.id AND lo.kind = ?
	)`)
	args = append(args, string(LLMKindSummary))

	q := `SELECT s.id, s.started_at_ms, s.ended_at_ms, s.cwd,
			s.first_prompt_text AS first_prompt
		FROM sessions s
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY ` + EffectiveTsExpr + ` DESC
		LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions missing summary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionDigestRow
	for rows.Next() {
		var r SessionDigestRow
		// LatestSummary stays at its zero value (Valid=false)
		// since the WHERE clause guarantees no summary exists.
		if err := rows.Scan(&r.ID, &r.StartedAtMs, &r.EndedAtMs, &r.Cwd, &r.FirstPrompt); err != nil {
			return nil, fmt.Errorf("scan missing-summary row: %w", err)
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

// LoadSessionStartCwd returns the session's start cwd — the directory
// the session was launched in. Distinct from sessions.cwd, which the
// migration-001 AFTER INSERT trigger keeps as the *latest* non-null
// cwd seen.
//
// Callers care about the start cwd specifically when generating
// `claude --resume <id>`: Claude indexes transcripts under
// ~/.claude/projects/<encoded-cwd>/ keyed off the cwd at session
// start, so resuming from any later directory the user cd'd into
// fails with "No conversation found".
//
// Backed by sessions.start_cwd (added in migration 015), populated
// by a parallel AFTER INSERT trigger that fires only on the first
// non-null cwd. Pre-015 stores backfill the column on migrate.
//
// Returns sql.NullString{} (not an error) when the session has no
// events with a recorded cwd — the caller decides whether to fall
// back to sessions.cwd or hide the resume button.
func LoadSessionStartCwd(ctx context.Context, db *sql.DB, sessionID string) (sql.NullString, error) {
	row := db.QueryRowContext(ctx,
		`SELECT start_cwd FROM sessions WHERE id = ?`,
		sessionID,
	)
	var cwd sql.NullString
	switch err := row.Scan(&cwd); {
	case errors.Is(err, sql.ErrNoRows):
		return sql.NullString{}, nil
	case err != nil:
		return sql.NullString{}, fmt.Errorf("scan start cwd: %w", err)
	}
	return cwd, nil
}

// LoadEventsForSession returns events for one session in chronological
// order. The `limit` argument follows three-state semantics:
//
//   - limit <= 0 → falls back to DefaultEventsPerSessionLimit (the
//     "I forgot to set a limit" path; rendering and summarize callers
//     usually take this branch).
//   - limit > 0  → at most `limit` rows.
//   - limit == LoadEventsForSessionUnbounded → no LIMIT clause; every
//     event in the session. Used by callers whose result is wrong if
//     truncated — the segmenter, primarily, where a missing tail
//     event would silently fix the final episode's ended_at_ms at
//     the wrong wall clock.
func LoadEventsForSession(ctx context.Context, db *sql.DB, sessionID string, limit int) ([]EventView, error) {
	switch {
	case limit == LoadEventsForSessionUnbounded:
		// no LIMIT clause
	case limit <= 0:
		limit = DefaultEventsPerSessionLimit
	}
	const projection = `event_id, kind, role, content_text, ts_source_ms, tool_name,
		        subagent_id, subagent_type, cwd`
	var (
		rows *sql.Rows
		err  error
	)
	if limit == LoadEventsForSessionUnbounded {
		rows, err = db.QueryContext(ctx,
			`SELECT `+projection+`
			   FROM events
			  WHERE session_id = ?
			  ORDER BY ts_source_ms ASC, rowid ASC`,
			sessionID,
		)
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT `+projection+`
			   FROM events
			  WHERE session_id = ?
			  ORDER BY ts_source_ms ASC, rowid ASC
			  LIMIT ?`,
			sessionID, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventView
	for rows.Next() {
		var e EventView
		if err := rows.Scan(&e.EventID, &e.Kind, &e.Role, &e.ContentText,
			&e.TsSourceMs, &e.ToolName, &e.SubagentID, &e.SubagentType, &e.Cwd); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LoadEventsForSessionUnbounded is the sentinel that tells
// LoadEventsForSession to skip the LIMIT clause and stream every
// event. Callers whose output is wrong under truncation (the episode
// segmenter is the canonical example: a truncated tail produces a
// final episode with the wrong ended_at_ms) pass this. The constant
// is a negative value so it's distinguishable from both "use the
// default" (limit <= 0 with the existing behaviour) and "this many
// rows please" (limit > 0).
const LoadEventsForSessionUnbounded = -1
