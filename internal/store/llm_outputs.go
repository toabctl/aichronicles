package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/internal/nullable"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/wire"
)

// LLMOutputKind and the LLMKind* constants are protocol-level
// vocabulary and live in internal/wire. These aliases keep the
// 126+ existing `store.LLMKindX` call sites working without forcing
// a one-shot rename; new code should reach for `wire.LLMKindX`
// directly. arch_review_2026_05_13 LOW: lift the type constants
// out of store so non-writer consumers (web handlers, CLI list
// commands) stop pulling internal/store just to read a string.
type LLMOutputKind = wire.LLMOutputKind

const (
	LLMKindSummary       = wire.LLMKindSummary
	LLMKindReflect       = wire.LLMKindReflect
	LLMKindPropose       = wire.LLMKindPropose
	LLMKindReflectWeekly = wire.LLMKindReflectWeekly
	LLMKindProposeVerify = wire.LLMKindProposeVerify
	LLMKindSkillRevision = wire.LLMKindSkillRevision
	LLMKindInduction     = wire.LLMKindInduction
	LLMKindChallenge     = wire.LLMKindChallenge
	LLMKindFacts         = wire.LLMKindFacts
	LLMKindSkillMerge    = wire.LLMKindSkillMerge
)

// LLMOutput mirrors one row of the llm_outputs table.
//
// SessionID is nil for multi-session outputs (e.g. weekly
// digests that span many sessions). InputTokens / OutputTokens
// are nil when the LLM provider didn't return usage counters
// (older models, certain test paths). All three flipped from
// sql.Null* to pointer types in the arch_review_2026_05_13
// MEDIUM #10 sweep so callers branch on `if x != nil` instead
// of unwrapping database/sql types.
type LLMOutput struct {
	ID           int64
	SessionID    *string
	Kind         LLMOutputKind
	Model        string
	PromptHash   string
	InputTokens  *int64
	OutputTokens *int64
	Body         string
	CreatedAtMs  int64
}

// SaveLLMOutput inserts out and returns its id. Idempotent on
// (kind, prompt_hash): re-calling with the same prompt yields the
// existing row and (false, nil) — never errors. This is the caching
// primitive the summarize/reflect/propose commands lean on to avoid
// re-paying for identical prompts.
//
// Body is scrubbed through redact.Outbound before storage. This is
// the LLM-output equivalent of IngestEnvelope's ErrRedactionRequired:
// the store layer enforces the redaction invariant for the write
// path that DOES NOT go through the daemon — anything an LLM
// hallucinates lands here, not via /v1/ingest, so we can't rely on
// the edge redactor having seen it. Callers do not need to scrub
// before calling; the input struct is not mutated.
func SaveLLMOutput(ctx context.Context, tx *sql.Tx, out *LLMOutput) (id int64, inserted bool, err error) {
	if out == nil {
		return 0, false, errors.New("SaveLLMOutput: nil output")
	}
	if out.Kind == "" {
		return 0, false, errors.New("SaveLLMOutput: kind is required")
	}
	if out.PromptHash == "" {
		return 0, false, errors.New("SaveLLMOutput: prompt_hash is required")
	}
	if out.Body == "" {
		return 0, false, errors.New("SaveLLMOutput: body is required")
	}

	scrubbedBody, _ := redact.Outbound(out.Body)

	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO llm_outputs(
			session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		out.SessionID, string(out.Kind), out.Model, out.PromptHash,
		out.InputTokens, out.OutputTokens, scrubbedBody, out.CreatedAtMs,
	)
	if err != nil {
		return 0, false, fmt.Errorf("insert llm_output: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// Duplicate (kind, prompt_hash) — look up the existing id.
		var existing int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM llm_outputs WHERE kind = ? AND prompt_hash = ?`,
			string(out.Kind), out.PromptHash,
		).Scan(&existing)
		if err != nil {
			return 0, false, fmt.Errorf("dedup lookup: %w", err)
		}
		return existing, false, nil
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("last insert id: %w", err)
	}
	return newID, true, nil
}

// LoadLLMOutputByHash returns the stored output for (kind, prompt_hash)
// or nil if none exists. Callers use this as an "have I already run
// this exact prompt?" probe before calling the LLM.
func LoadLLMOutputByHash(ctx context.Context, db *sql.DB, kind LLMOutputKind, promptHash string) (*LLMOutput, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+llmOutputColumns+`
		 FROM llm_outputs
		 WHERE kind = ? AND prompt_hash = ?`,
		string(kind), promptHash,
	)
	out, err := scanLLMOutput(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

// LoadLLMOutputByID returns the row with the given primary key,
// or nil if no row matches. Used by callers that have a stable
// reference to a specific output (e.g. `propose add --output-id`)
// and want to act on that exact row regardless of recency.
func LoadLLMOutputByID(ctx context.Context, db *sql.DB, id int64) (*LLMOutput, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+llmOutputColumns+`
		 FROM llm_outputs
		 WHERE id = ?`,
		id,
	)
	out, err := scanLLMOutput(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

// LastLLMOutputCreatedAt returns created_at_ms of the most recent
// llm_outputs row of the given kind, or 0 if no row exists. Used
// by the daemon's meta-analysis sweeper to gate cadence — "fire
// kind=X if it's been longer than cadence[X] since the last run."
//
// Returns 0 (not an error) for "no row yet" so the cadence check
// reads as `now - 0 > cadence` → fire immediately, which is the
// natural semantics for the first-run case.
func LastLLMOutputCreatedAt(ctx context.Context, db *sql.DB, kind LLMOutputKind) (int64, error) {
	if kind == "" {
		return 0, errors.New("LastLLMOutputCreatedAt: kind is required")
	}
	var ts int64
	err := db.QueryRowContext(ctx,
		`SELECT created_at_ms FROM llm_outputs WHERE kind = ? ORDER BY created_at_ms DESC LIMIT 1`,
		string(kind),
	).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("last created_at for %s: %w", kind, err)
	}
	return ts, nil
}

// HasLLMOutputForSession returns true when at least one row exists
// in llm_outputs with session_id = sessionID AND kind = kind.
//
// Cheap existence check used by the daemon's pipeline sweeper to
// decide whether the per-session phases (summarize / induction /
// facts) need to run for a candidate session. Avoids building a
// prompt + hashing it just to discover there's already a row —
// the LIMIT-1 row read is essentially free against the
// idx_llm_outputs_session_kind partial index.
func HasLLMOutputForSession(ctx context.Context, db *sql.DB, sessionID string, kind LLMOutputKind) (bool, error) {
	if sessionID == "" {
		return false, errors.New("HasLLMOutputForSession: session_id is required")
	}
	if kind == "" {
		return false, errors.New("HasLLMOutputForSession: kind is required")
	}
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM llm_outputs WHERE session_id = ? AND kind = ? LIMIT 1`,
		sessionID, string(kind),
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check llm_output existence: %w", err)
	}
	return true, nil
}

// LLMOutputFilter composes a list-query. Any zero-value field is
// omitted from the WHERE clause, so a zero-value filter lists every
// row (subject to Limit).
type LLMOutputFilter struct {
	SessionID string        // exact match; empty means "no session filter"
	Kind      LLMOutputKind // exact match; empty means "no kind filter"
	// Limit caps the result set, newest-first by created_at_ms. A
	// non-positive value uses DefaultLLMOutputsListLimit.
	Limit int
}

// DefaultLLMOutputsListLimit is the cap applied when a caller passes
// a non-positive Limit through LLMOutputFilter. 50 balances "see a
// history at a glance" against "don't slurp the whole table into a
// CLI buffer".
const DefaultLLMOutputsListLimit = 50

// LoadLLMOutputs is the generic read path for llm_outputs. Combines
// optional session_id and kind filters with a newest-first ORDER BY
// and a LIMIT so CLI listings stay bounded.
//
// Null session_id rows (multi-session outputs from reflect/propose)
// are included when filter.SessionID is empty. When filter.SessionID
// is set, rows with NULL session_id are naturally excluded by the
// equality.
func LoadLLMOutputs(ctx context.Context, db *sql.DB, filter LLMOutputFilter) ([]LLMOutput, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultLLMOutputsListLimit
	}

	var where []string
	var args []any
	if filter.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, string(filter.Kind))
	}
	q := `SELECT ` + llmOutputColumns + ` FROM llm_outputs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at_ms DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query llm_outputs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LLMOutput
	for rows.Next() {
		item, err := scanLLMOutput(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

// LoadLLMOutputsForSession returns every output attached to a given
// session, newest first. Empty slice when there are none.
func LoadLLMOutputsForSession(ctx context.Context, db *sql.DB, sessionID string) ([]LLMOutput, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+llmOutputColumns+`
		 FROM llm_outputs
		 WHERE session_id = ?
		 ORDER BY created_at_ms DESC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query llm_outputs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LLMOutput
	for rows.Next() {
		item, err := scanLLMOutput(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

// LoadSummariesIndexedByID returns the most-recent kind='summary'
// output for each session in sessionIDs, keyed by session_id.
// Sessions without any cached summary are absent from the returned
// map (no entry rather than a zero-value entry, so callers can
// distinguish "not yet summarised" from "summary with empty body").
//
// One indexed query — the alternative of calling
// LoadLLMOutputsForSession per session is N+1 and the sessions
// list / search results call this on every render. ORDER BY
// created_at_ms DESC means the first row we see for any given
// session wins, which is exactly the newest summary.
//
// Empty input returns an empty map and no query.
func LoadSummariesIndexedByID(ctx context.Context, db *sql.DB, sessionIDs []string) (map[string]LLMOutput, error) {
	if len(sessionIDs) == 0 {
		return map[string]LLMOutput{}, nil
	}

	placeholders, args := inPlaceholders(sessionIDs)
	args = append(args, string(LLMKindSummary))

	q := `SELECT ` + llmOutputColumns + `
		FROM llm_outputs
		WHERE session_id IN (` + placeholders + `) AND kind = ?
		ORDER BY created_at_ms DESC`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]LLMOutput, len(sessionIDs))
	for rows.Next() {
		item, err := scanLLMOutput(rows)
		if err != nil {
			return nil, err
		}
		// Newest-first ORDER BY: first row per session wins.
		// Skip session_id NULL defensively — should never appear
		// here since we filter by IN(non-null-list), but a NULL
		// would map to the empty key and clobber.
		if item.SessionID == nil {
			continue
		}
		if _, seen := out[*item.SessionID]; seen {
			continue
		}
		out[*item.SessionID] = *item
	}
	return out, rows.Err()
}

// rowScanner unifies *sql.Row and *sql.Rows for scanLLMOutput.
type rowScanner interface {
	Scan(dest ...any) error
}

// llmOutputColumns is the canonical column list for SELECTs that feed
// scanLLMOutput. Keep this string and the scan helper in lockstep;
// the projection appears in six loaders across this package and
// induction.go, and a hand-typed deviation would silently column-
// shift the scan.
const llmOutputColumns = `id, session_id, kind, model, prompt_hash,
	input_tokens, output_tokens, body, created_at_ms`

func scanLLMOutput(r rowScanner) (*LLMOutput, error) {
	var (
		o            LLMOutput
		kind         string
		sessionID    sql.NullString
		inputTokens  sql.NullInt64
		outputTokens sql.NullInt64
	)
	if err := r.Scan(
		&o.ID, &sessionID, &kind, &o.Model, &o.PromptHash,
		&inputTokens, &outputTokens, &o.Body, &o.CreatedAtMs,
	); err != nil {
		return nil, err
	}
	o.SessionID = nullable.StringPtr(sessionID)
	o.InputTokens = nullable.Int64Ptr(inputTokens)
	o.OutputTokens = nullable.Int64Ptr(outputTokens)
	o.Kind = LLMOutputKind(kind)
	return &o, nil
}
