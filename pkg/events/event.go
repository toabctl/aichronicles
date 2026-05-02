package events

import "errors"

// Event is what flows through a Pipeline: a validated, redacted
// Envelope plus the verbatim source bytes (for Sinks that want to
// persist source-of-truth — e.g. the SQLite Sink writes them to
// raw_envelopes.envelope_json) plus any Extractions the Pipeline
// computed before passing to the Sink. Sinks see one fully-
// decorated value.
//
// Raw is intentionally bytes rather than the original Envelope-
// json bytes derived by re-marshaling: the daemon HTTP path has
// the verbatim request body in hand and round-tripping through
// json.Marshal would produce semantically-equal but byte-different
// output. Sources that translate from a non-Envelope shape (e.g.
// Claude .jsonl transcripts) marshal the Envelope themselves and
// set Raw to that; both paths converge on "Raw is what we'd want
// to replay."
type Event struct {
	Envelope    *Envelope
	Raw         []byte
	Extractions []Extraction
}

// Result is what Sink.Write returns and Pipeline.Process returns.
// Deduped=true means the Sink saw an existing row with this
// EventID and chose idempotent no-op rather than INSERT.
type Result struct {
	EventID   string
	SessionID string
	Deduped   bool
}

// Stats is the aggregate Pipeline.Run returns. Processed counts
// every event accepted by the Sink; Deduped is the subset that
// the Sink reported as already-present; Errors counts events the
// pipeline could not handle (per-event isolation — one bad event
// does not abort the run).
type Stats struct {
	Processed int
	Deduped   int
	Errors    int
}

// ErrRedactionRequired is returned by Pipeline.Process when an
// envelope reaches it without Redaction.Applied=true. Sinks
// implementing the SQLite-backed contract enforce the same
// invariant as defense-in-depth so a forgetful Pipeline caller
// can't quietly write an unredacted envelope.
var ErrRedactionRequired = errors.New("envelope.redaction.applied must be true")
