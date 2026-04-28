package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"
)

// InductionSweeper is a daemon-resident background goroutine that
// periodically calls a sweep callback (typically wrapping
// cli.RunInductionSweep) so newly-idle sessions get auto-induced
// without the user having to schedule a cron job.
//
// Online AWM (Wang et al., 2024) is fundamentally about the
// induction firing while the agent runs — not as an offline
// batch job. Manual `aichronicles induction sweep` is a fine
// stopgap, but the actual contract is "session ends, skill
// surfaces", and that needs in-process scheduling.
//
// Disabled-by-default: the daemon's run() only spawns this when
// cfg.Induction.Enabled is true. Each tick is panic-recovered so
// a malformed prompt or LLM hiccup can't strand the goroutine.
//
// Safe shutdown: the goroutine exits when ctx cancels.
type InductionSweeper struct {
	// Interval is how often Sweep is called. Smaller intervals
	// make induction more responsive but don't help when the
	// candidate query keeps returning empty.
	Interval time.Duration

	// Sweep is the work callback. Returning an error from one
	// invocation does NOT abort the loop — the next tick fires
	// regardless. The callback is responsible for its own per-
	// session error isolation (so one bad session doesn't kill
	// the rest of a sweep).
	Sweep func(ctx context.Context) error

	// Log is the structured logger. Every tick — fired, success
	// or failure — emits one slog line so an operator can
	// distinguish "the sweeper is working but finds nothing" from
	// "the sweeper hasn't run".
	Log *slog.Logger
}

// Run blocks until ctx cancels. Fires Sweep immediately on start
// (so the daemon doesn't wait a full Interval before its first
// induction), then on every Interval tick.
//
// Panic recovery is per-tick: a panic during one Sweep call is
// logged and the loop continues. That's the right blast radius —
// the alternative (panic propagates out, daemon crashes) would
// take ingest down for what's a non-essential side feature.
func (s *InductionSweeper) Run(ctx context.Context) {
	if s.Sweep == nil {
		s.Log.Warn("induction sweeper: no Sweep callback, exiting")
		return
	}
	if s.Interval <= 0 {
		s.Log.Warn("induction sweeper: non-positive interval, exiting", "interval", s.Interval)
		return
	}

	s.Log.Info("induction sweeper started", "interval", s.Interval)
	defer s.Log.Info("induction sweeper stopped")

	// Fire once immediately to drain any backlog from a daemon
	// that was offline. Then settle into the periodic cadence.
	s.tick(ctx)

	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tick(ctx)
		}
	}
}

// tick fires one Sweep, recovering from a panic. Stays a private
// method (rather than inline in Run) so the recover() boundary
// is unambiguous and tests can drive it directly.
func (s *InductionSweeper) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("induction sweep panicked, continuing", "panic", r)
		}
	}()
	if err := s.Sweep(ctx); err != nil {
		// context.Canceled is the expected path during shutdown —
		// don't log it at warn level (the operator already saw
		// the shutdown line).
		if errors.Is(err, context.Canceled) {
			return
		}
		s.Log.Warn("induction sweep failed", "err", err)
		return
	}
}

// DiscardWriter is a tiny helper that exposes io.Discard as a
// strongly-typed value. The induction CLI takes io.Writer
// arguments; the daemon doesn't have a stdout/stderr to surface
// induction output to, so it routes everything to /dev/null and
// relies on the slog calls inside Sweep for telemetry.
//
// Local definition (rather than reaching for io.Discard at every
// caller) keeps the daemon package self-contained without
// importing io purely for the package-level value.
var DiscardWriter io.Writer = io.Discard
