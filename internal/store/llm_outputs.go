package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// LLMOutputKind is the discriminator for llm_outputs.kind. Application
// code is the only place that enforces the vocabulary; the DB column
// is free text so new kinds (Block D+ features) don't need a migration.
type LLMOutputKind string

const (
	LLMKindSummary LLMOutputKind = "summary"
	LLMKindReflect LLMOutputKind = "reflect"
	LLMKindPropose LLMOutputKind = "propose"
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
func SaveLLMOutput(tx *sql.Tx, out *LLMOutput) (id int64, inserted bool, err error) {
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

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO llm_outputs(
			session_id, kind, model, prompt_hash,
			input_tokens, output_tokens, body, created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		out.SessionID, string(out.Kind), out.Model, out.PromptHash,
		out.InputTokens, out.OutputTokens, out.Body, out.CreatedAtMs,
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
		err := tx.QueryRow(
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
func LoadLLMOutputByHash(db *sql.DB, kind LLMOutputKind, promptHash string) (*LLMOutput, error) {
	row := db.QueryRow(
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

// LoadLLMOutputsForSession returns every output attached to a given
// session, newest first. Empty slice when there are none.
func LoadLLMOutputsForSession(db *sql.DB, sessionID string) ([]LLMOutput, error) {
	rows, err := db.Query(
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
