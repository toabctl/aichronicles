package store

import "github.com/toabctl/aichronicles/internal/redact"

// This file holds the store layer's outbound-redaction invariant.
//
// Ingest has its own enforcement: IngestEnvelope rejects an envelope
// whose Redaction.Applied is false, because everything arriving there
// passed through the daemon's edge redactor. Nothing else in the
// store gets that guarantee.
//
// The LLM-authored write paths bypass ingest entirely. A model asked
// to summarise a session will happily transcribe a token it saw into
// an evidence quote, an episode intent, a link rationale or a skill
// trigger — CLAUDE.md §7 even instructs it to quote real observed
// values. Those strings go straight from the API response to SQLite,
// so the store is the last place that can scrub them.
//
// SaveLLMOutput enforced this for llm_outputs.body and documented the
// reasoning, but four sibling paths writing the same class of text
// did not. Rather than repeat the call at each site, both helpers
// below name the invariant once so a new write path has an obvious
// thing to reach for — and grepping scrubStored shows every place the
// rule is applied.
//
// Scrubbing at write time is deliberately lossy and deliberately
// silent: a secret in a model-authored quote is not something the
// user needs a prompt about, and storing it would be worse than
// storing the marker. Retroactive cleanup of rows written before a
// detector existed is Scrub's job, not this one's.

// scrubStored returns s with any detected credential rewritten to its
// <redacted:kind> marker. Use on every free-text column fed by model
// output before it reaches SQLite.
func scrubStored(s string) string {
	if s == "" {
		return s
	}
	out, _ := redact.Outbound(s)
	return out
}

// scrubStoredPtr is scrubStored for nullable columns. A nil pointer
// stays nil — "no value" is distinct from "empty value" in the
// schema, and collapsing the two would change what NULL means for
// callers that branch on it.
func scrubStoredPtr(s *string) *string {
	if s == nil || *s == "" {
		return s
	}
	cleaned := scrubStored(*s)
	return &cleaned
}

// scrubStoredList scrubs each element of a slice destined for a
// JSON-encoded column, returning a new slice so the caller's input is
// not mutated.
//
// Scrub the elements, never the encoded JSON: a secret containing a
// quote or backslash is escaped by the encoder, and the detector
// would not match the escaped form. Same ordering trap as scrubbing
// before truncation.
func scrubStoredList(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = scrubStored(s)
	}
	return out
}

// scrubStoredExamples scrubs both free-text fields of each example.
func scrubStoredExamples(in []SkillExample) []SkillExample {
	if len(in) == 0 {
		return in
	}
	out := make([]SkillExample, len(in))
	for i, ex := range in {
		out[i] = SkillExample{
			Input:  scrubStored(ex.Input),
			Output: scrubStored(ex.Output),
		}
	}
	return out
}
