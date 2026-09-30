package wire

// SemanticFact is the wire shape for one semantic_facts row, as
// returned by /v1/facts (?subject= for one subject, none for the
// most recent across subjects). Maps from
// store.SemanticFact at the handler boundary.
type SemanticFact struct {
	ID                int64   `json:"id"`
	SourceLLMOutputID int64   `json:"source_llm_output_id"`
	Subject           string  `json:"subject"`
	Predicate         string  `json:"predicate"`
	Object            string  `json:"object"`
	Confidence        float64 `json:"confidence"`
	EvidenceSessionID *string `json:"evidence_session_id,omitempty"`
	EvidenceQuote     *string `json:"evidence_quote,omitempty"`
	AssertedAtMs      int64   `json:"asserted_at_ms"`
}

// FactSubjectsResponse is the body for /v1/facts/subjects.
type FactSubjectsResponse struct {
	Subjects []string `json:"subjects"`
}

// FactsResponse is the body for /v1/facts (with or without
// ?subject=). NextCursor (via PageResponse) pages forward.
type FactsResponse struct {
	Facts []SemanticFact `json:"facts"`
	PageResponse
}

// RecommendedFactPredicates is a non-binding suggested vocabulary the
// facts-induction prompt advertises to the LLM. It lives in wire, like
// LLMOutputKind, because readers outside the store (the MCP
// project-context view) rank facts by it. The schema does NOT
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
