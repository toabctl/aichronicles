package events

import (
	"context"
	"iter"
)

// Source produces a stream of events. The HTTP daemon (one event
// per request), the Claude / Gemini transcript importers (many
// events per file walk), and any future bridge each implement
// this. iter.Seq2 (Go 1.23+) is the canonical stream shape: yield
// (Event, nil) to consume an event, yield (Event{}, err) to halt
// the iteration with an error. Sources MUST close the iterator on
// EOF or context cancellation.
//
// Sources are responsible for applying redaction before yielding
// (see the Redactor interface). The Pipeline verifies
// env.Redaction.Applied=true and refuses to forward un-redacted
// events to the Sink — this is the layered defense the project's
// "no unredacted secrets" invariant relies on.
type Source interface {
	Events(ctx context.Context) iter.Seq2[Event, error]
}
