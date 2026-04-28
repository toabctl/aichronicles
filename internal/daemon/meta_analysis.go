package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// MetaAnalysisSweeper is a daemon-resident background goroutine
// that periodically calls a sweep callback (typically wrapping
// cli.RunMetaAnalysis) so multi-session meta-analyses (propose /
// reflect / challenge / digest_weekly / skill_revision) auto-run
// on their configured cadences without the user having to schedule
// a cron job.
//
// Distinct from InductionSweeper: induction is per-session driven
// by "session settled," fires every tick. Meta-analysis is per-
// kind, gated on "elapsed since last run >= cadence[kind]". One
// hourly tick for the meta-sweeper is plenty since cadences are
// measured in days.
//
// Disabled-by-default: the daemon's run() only spawns this when
// cfg.MetaAnalysis.Enabled is true. Each tick is panic-recovered
// so a malformed prompt or LLM hiccup can't strand the goroutine.
//
// Safe shutdown: the goroutine exits when ctx cancels.
type MetaAnalysisSweeper struct {
	// Interval is how often Sweep is called. Smaller intervals
	// make cadence enforcement more responsive but don't help
	// when no kind is overdue. 1h is the sweet spot.
	Interval time.Duration

	// Sweep is the work callback. Returning an error from one
	// invocation does NOT abort the loop — the next tick fires
	// regardless. The callback is responsible for its own per-
	// kind error isolation (so one bad kind doesn't kill the
	// rest of a sweep).
	Sweep func(ctx context.Context) error

	// Log is the structured logger. Every tick — fired, success
	// or failure — emits one slog line so an operator can
	// distinguish "the sweeper is working but everything is
	// still in cadence" from "the sweeper hasn't run".
	Log *slog.Logger
}

// Run blocks until ctx cancels. Fires Sweep immediately on start
// (so the daemon doesn't wait a full Interval before its first
// pass), then on every Interval tick.
//
// Panic recovery is per-tick: a panic during one Sweep call is
// logged and the loop continues. That's the right blast radius —
// the alternative (panic propagates out, daemon crashes) would
// take ingest down for what's a non-essential side feature.
func (s *MetaAnalysisSweeper) Run(ctx context.Context) {
	if s.Sweep == nil {
		s.Log.Warn("meta-analysis sweeper: no Sweep callback, exiting")
		return
	}
	if s.Interval <= 0 {
		s.Log.Warn("meta-analysis sweeper: non-positive interval, exiting", "interval", s.Interval)
		return
	}

	s.Log.Info("meta-analysis sweeper started", "interval", s.Interval)
	defer s.Log.Info("meta-analysis sweeper stopped")

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
// method (rather than inline in Run) so the recover() boundary is
// unambiguous and tests can drive it directly.
func (s *MetaAnalysisSweeper) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("meta-analysis sweep panicked, continuing", "panic", r)
		}
	}()
	if err := s.Sweep(ctx); err != nil {
		// context.Canceled is the expected path during shutdown —
		// don't log it at warn level (the operator already saw
		// the shutdown line).
		if errors.Is(err, context.Canceled) {
			return
		}
		s.Log.Warn("meta-analysis sweep failed", "err", err)
		return
	}
}
