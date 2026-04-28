package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SemanticFact is one row of the semantic_facts table — a typed
// project-level fact derived from a session by the facts-induction
// LLM call. Distinguished from episodic memory (events) and
// procedural memory (skills, workflows, propose):
//
//   - episodic — what happened, in order, exactly as it happened
//   - procedural — how to accomplish a kind of task
//   - semantic — what is true about the world right now
//
// SemanticFact is the third leg. Subject is typically a cwd path
// for v1 (project-level facts), Predicate names the relation
// (uses_language_version, runs_tests_via, …), Object holds the
// value.
type SemanticFact struct {
	ID                int64
	SourceLLMOutputID int64
	Subject           string
	Predicate         string
	Object            string
	Confidence        float64
	EvidenceSessionID sql.NullString
	EvidenceQuote     sql.NullString
	AssertedAtMs      int64
}

// RecommendedFactPredicates is a non-binding suggested vocabulary the
// facts-induction prompt advertises to the LLM. The schema does NOT
// enforce these — free-form predicates are valid — but stable
// retrieval queries depend on the LLM picking from a small set.
// Adding a predicate here is a code change, not a migration.
//
// kebab-case under-score-separated to match the rest of the
// project's identifier conventions (skill_load extraction kind,
// workflow_step action_template tokens, etc.).
var RecommendedFactPredicates = []string{
	"uses_language_version",   // "Go 1.26", "Python 3.12"
	"runs_tests_via",          // "go test ./...", "pytest -xvs"
	"runs_build_via",          // "go build ./...", "make build"
	"runs_lint_via",           // "golangci-lint run ./...", "ruff check ."
	"deploys_to",              // "staging", "k8s cluster prod-1"
	"uses_dependency",         // "modernc.org/sqlite", "anthropic/claude-sdk"
	"key_directory",           // "internal/store", "src/api"
	"git_branch_convention",   // "feature branches off main"
	"commit_convention",       // "conventional commits"
	"documentation_at",        // "docs/explanation/threat-model.md"
	"requires_setup_step",     // "run aichronicles setup claude-code first"
	"requires_environment",    // "ANTHROPIC_API_KEY", "DATABASE_URL"
	"runs_via_command",        // generic catchall when the action doesn't fit a more specific predicate
	"primary_language",        // "Go", "TypeScript"
	"build_artefact_location", // "./bin/", "dist/"
}

// SaveSemanticFact upserts one fact into semantic_facts. PK is the
// (subject, predicate, object) UNIQUE constraint; on conflict the
// asserted_at_ms / confidence / evidence pointer / source_llm_output_id
// are all refreshed (latest evidence wins). Conflicting object values
// for the same (subject, predicate) coexist as separate rows so the
// truth is never silently overwritten — the caller picks by
// asserted_at_ms when retrieving.
//
// Returns the row id of the inserted-or-updated fact.
func SaveSemanticFact(ctx context.Context, db *sql.DB, f SemanticFact) (int64, error) {
	if f.SourceLLMOutputID <= 0 {
		return 0, errors.New("SaveSemanticFact: source_llm_output_id is required")
	}
	if f.Subject == "" {
		return 0, errors.New("SaveSemanticFact: subject is required")
	}
	if f.Predicate == "" {
		return 0, errors.New("SaveSemanticFact: predicate is required")
	}
	if f.Object == "" {
		return 0, errors.New("SaveSemanticFact: object is required")
	}
	if f.AssertedAtMs <= 0 {
		return 0, errors.New("SaveSemanticFact: asserted_at_ms is required")
	}
	if f.Confidence < 0 || f.Confidence > 1 {
		return 0, fmt.Errorf("SaveSemanticFact: confidence %v out of [0,1]", f.Confidence)
	}

	const q = `
INSERT INTO semantic_facts(
    source_llm_output_id, subject, predicate, object,
    confidence, evidence_session_id, evidence_quote, asserted_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(subject, predicate, object) DO UPDATE SET
    source_llm_output_id = excluded.source_llm_output_id,
    confidence           = excluded.confidence,
    evidence_session_id  = excluded.evidence_session_id,
    evidence_quote       = excluded.evidence_quote,
    asserted_at_ms       = excluded.asserted_at_ms
RETURNING id`
	var id int64
	if err := db.QueryRowContext(ctx, q,
		f.SourceLLMOutputID, f.Subject, f.Predicate, f.Object,
		f.Confidence, f.EvidenceSessionID, f.EvidenceQuote, f.AssertedAtMs,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("save semantic_fact: %w", err)
	}
	return id, nil
}

// LoadFactsForSubject returns every semantic_facts row for the given
// subject, ordered by predicate then asserted_at_ms DESC. Limit ≤0
// falls back to 100 — enough to dump everything we know about a
// typical project, not so much that pathological inputs DOS the
// caller.
//
// Empty result is fine: a fresh project simply has no facts asserted
// yet. Callers render an empty-state message rather than treating it
// as an error.
func LoadFactsForSubject(ctx context.Context, db *sql.DB, subject string, limit int) ([]SemanticFact, error) {
	if subject == "" {
		return nil, errors.New("LoadFactsForSubject: subject is required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, source_llm_output_id, subject, predicate, object,
		        confidence, evidence_session_id, evidence_quote, asserted_at_ms
		   FROM semantic_facts
		  WHERE subject = ?
		  ORDER BY predicate ASC, asserted_at_ms DESC
		  LIMIT ?`,
		subject, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query semantic_facts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSemanticFacts(rows)
}

// LoadRecentFacts returns the N most recently asserted facts across
// all subjects, newest first. Used by `aichronicles facts list` and
// the introspection UI to scan the corpus.
//
// limit ≤0 falls back to 50.
func LoadRecentFacts(ctx context.Context, db *sql.DB, limit int) ([]SemanticFact, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, source_llm_output_id, subject, predicate, object,
		        confidence, evidence_session_id, evidence_quote, asserted_at_ms
		   FROM semantic_facts
		  ORDER BY asserted_at_ms DESC
		  LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent facts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSemanticFacts(rows)
}

// FactSubjectsLike returns distinct subject strings whose value
// contains needle (case-insensitive). Used by the MCP tool for
// "find me everything about /work/aichronicles" or "anything with
// systemd in the path". Results are deduped + sorted.
//
// limit ≤0 falls back to 30. Empty needle returns an error rather
// than scanning the universe.
func FactSubjectsLike(ctx context.Context, db *sql.DB, needle string, limit int) ([]string, error) {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return nil, errors.New("FactSubjectsLike: needle is required")
	}
	if limit <= 0 {
		limit = 30
	}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT subject
		   FROM semantic_facts
		  WHERE subject LIKE ? COLLATE NOCASE
		  ORDER BY subject ASC
		  LIMIT ?`,
		"%"+needle+"%", limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query distinct subjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan subject: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// scanSemanticFacts iterates rows from a SELECT * shaped query and
// produces the slice. Shared between LoadFactsForSubject and
// LoadRecentFacts so the column order stays in lockstep.
func scanSemanticFacts(rows *sql.Rows) ([]SemanticFact, error) {
	var out []SemanticFact
	for rows.Next() {
		var f SemanticFact
		if err := rows.Scan(
			&f.ID, &f.SourceLLMOutputID, &f.Subject, &f.Predicate, &f.Object,
			&f.Confidence, &f.EvidenceSessionID, &f.EvidenceQuote, &f.AssertedAtMs,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
