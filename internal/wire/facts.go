package wire

// SemanticFact is the wire shape for one semantic_facts row, as
// returned by /v1/facts and /v1/facts/recent. Maps from
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

// FactsResponse is the body for /v1/facts (?subject=...) and
// /v1/facts/recent.
type FactsResponse struct {
	Facts []SemanticFact `json:"facts"`
}
