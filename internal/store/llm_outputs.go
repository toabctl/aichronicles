package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/toabctl/aichronicles/pkg/redact"
)

// LLMOutputKind is the discriminator for llm_outputs.kind. Application
// code is the only place that enforces the vocabulary; the DB column
// is free text so new kinds (Block D+ features) don't need a migration.
type LLMOutputKind string

const (
	LLMKindSummary       LLMOutputKind = "summary"
	LLMKindReflect       LLMOutputKind = "reflect"
	LLMKindPropose       LLMOutputKind = "propose"
	LLMKindReflectWeekly LLMOutputKind = "reflect_weekly"
	// LLMKindProposeVerify is the cached output of the critic LLM
	// pass that `propose apply` runs before writing a SKILL.md
	// (Voyager-style verification gate). One row per (proposal-id,
	// skill-name) pair so re-running apply on the same skill is
	// free.
	LLMKindProposeVerify LLMOutputKind = "propose_verify"
	// LLMKindSkillRevision is the cached output of `aichronicles
	// skills evolve` — a revision of an existing SKILL.md
	// grounded in the failure events the staleness detector
	// flagged. One row per (skill-name, current-skill-md-hash)
	// so re-running on the same SKILL contents is free; a hand-
	// edit to the SKILL.md invalidates the cache automatically.
	LLMKindSkillRevision LLMOutputKind = "skill_revision"
	// LLMKindInduction is the cached output of online induction
	// — single-session propose triggered the moment a session
	// idles out. One row per (session_id, prompt-hash) so
	// re-running on the same session contents hits the cache.
	// Distinguished from LLMKindPropose so the CLI listing can
	// segregate "skills surfaced from one session by the auto
	// trigger" from "skills surfaced from a multi-session window
	// by the user".
	LLMKindInduction LLMOutputKind = "induction"
	// LLMKindChallenge is the cached output of `propose
	// --challenge`: forward-looking next-problem suggestions
	// derived from the same digest list propose uses, plus open
	// threads from prior sessions. Voyager's automatic-curriculum
	// analog. Separate from LLMKindPropose so the CLI listing
	// distinguishes "skills surfaced from past patterns" from
	// "challenges I should tackle next".
	LLMKindChallenge LLMOutputKind = "challenge"
	// LLMKindWorkflow is the cached output of single-session
	// workflow induction (AWM — Agent Workflow Memory; Wang et al.
	// 2024, arXiv:2409.07429). Distinguished from LLMKindInduction
	// (which produces SKILL-shaped artefacts that the user may apply
	// to disk via `propose apply`): a workflow is a deliberately
	// ABSTRACT procedural recipe — drop concrete URLs/IDs/file paths,
	// keep the procedure shape — that lives in the database for
	// retrieval at task-planning time, not on disk as a skill.
	//
	// One row per (session_id, prompt-hash) so re-running on the
	// same session is free.
	LLMKindWorkflow LLMOutputKind = "workflow"
	// LLMKindFacts is the cached output of single-session SEMANTIC
	// fact induction. The LLM extracts typed (subject, predicate,
	// object) triples from the session — project-level facts like
	// "uses Go 1.26", "runs tests via go test ./..." — and the
	// caller persists them into the semantic_facts table for typed
	// retrieval. The llm_outputs row holds the raw LLM reply for
	// caching + auditability; the truth lives in semantic_facts.
	LLMKindFacts LLMOutputKind = "facts"
)

// LLMOutput mirrors one row of the llm_outputs table. Callers
// populate SessionID.Valid=false for multi-session outputs.
type LLMOutput struct {
	ID           int64
	SessionID    sql.NullString
	Kind         LLMOutputKind
	Model        string
	PromptHash   string
	InputTokens  sql.NullInt64
	OutputTokens sql.NullInt64
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
		`SELECT id, session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
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
// reference to a specific output (e.g. `propose apply --output-id`)
// and want to act on that exact row regardless of recency.
func LoadLLMOutputByID(ctx context.Context, db *sql.DB, id int64) (*LLMOutput, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
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
	q := `SELECT id, session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
		 FROM llm_outputs`
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
		`SELECT id, session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
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

	placeholders := strings.Repeat(",?", len(sessionIDs))[1:]
	args := make([]any, 0, len(sessionIDs)+1)
	for _, id := range sessionIDs {
		args = append(args, id)
	}
	args = append(args, string(LLMKindSummary))

	q := `SELECT id, session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
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
		if !item.SessionID.Valid {
			continue
		}
		if _, seen := out[item.SessionID.String]; seen {
			continue
		}
		out[item.SessionID.String] = *item
	}
	return out, rows.Err()
}

// rowScanner unifies *sql.Row and *sql.Rows for scanLLMOutput.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanLLMOutput(r rowScanner) (*LLMOutput, error) {
	var o LLMOutput
	var kind string
	if err := r.Scan(
		&o.ID, &o.SessionID, &kind, &o.Model, &o.PromptHash,
		&o.InputTokens, &o.OutputTokens, &o.Body, &o.CreatedAtMs,
	); err != nil {
		return nil, err
	}
	o.Kind = LLMOutputKind(kind)
	return &o, nil
}
