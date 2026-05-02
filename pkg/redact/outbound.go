package redact

// Outbound is the single choke point any code that ships stored text
// to an external LLM (Block B: summarize, reflect, propose) is
// expected to call. It re-runs the default scanner against whatever
// string is about to leave the process, independent of whether the
// source event was already scrubbed at events.
//
// Defense-in-depth reasons to re-scan here:
//
//   - A detector can be added after an event was ingested. Audit +
//     scrub can catch most of those, but a call hot on the heels of
//     a detector change may run before scrub does.
//   - A future code path might assemble the outgoing prompt from
//     multiple events and intermediate derivations where a cross-
//     boundary secret appears only after concatenation.
//   - Third-party extensions to the store (imports, migrations) may
//     land text that dodged the ingest invariant.
//
// Outbound is deliberately pure — no I/O, no state, no logging — so
// it can be used safely inside tight inner loops of prompt assembly.
// It returns the scrubbed string AND the list of patterns that fired,
// so the caller can decline to send the request entirely if, for
// example, an anthropic_api_key just appeared in what they were about
// to send back to Anthropic.
func Outbound(s string) (string, []string) {
	findings := Default().Scan(s)
	return Replace(s, findings)
}

// MustClean is the stricter variant: it returns the scrubbed string
// only if nothing fired. If any pattern matched, it returns "" and
// the list of pattern names. Useful for callers that would rather
// abort than risk sending a partial prompt, e.g. a "summarize this
// session" request where the presence of a credential means the
// summarization should be skipped and flagged.
func MustClean(s string) (string, []string) {
	out, patterns := Outbound(s)
	if len(patterns) > 0 {
		return "", patterns
	}
	return out, nil
}
