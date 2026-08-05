package cli

import (
	"context"
	"time"
)

// adminOpTimeout bounds the maintenance operations that rescan or
// rewrite the whole store: scrub, prune, vacuum, audit.
//
// Generous on purpose. These are O(database size), not O(request):
// scrub rescans every raw envelope, extraction and LLM output, and
// vacuum rewrites the file. On a multi-GB store that is minutes.
//
// The apiclient's default 30s bound is right for ordinary reads and
// badly wrong here — `aichronicles scrub` against a real corpus
// failed with "Client.Timeout exceeded while awaiting headers", which
// is the size at which scrub actually matters. An hour is far past
// any plausible honest run while still bounding a genuinely wedged
// daemon, so the CLI never hangs forever.
const adminOpTimeout = time.Hour

// withAdminTimeout derives the context for a long maintenance call.
// Separate from the LLM command timeouts because the bound is set by
// corpus size rather than by a provider's latency.
func withAdminTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, adminOpTimeout)
}
