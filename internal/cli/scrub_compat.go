package cli

import "context"

// legacyScrubCtx returns context.Background — extracted so the
// legacy RunScrub wrapper in scrub.go can keep its tiny shape
// while still using a real context. Tests that need cancellation
// should switch to calling store.Scrub directly with their own
// ctx.
func legacyScrubCtx() context.Context {
	return context.Background()
}
