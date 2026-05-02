package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Pipeline ties Source → Extractors → Sink. Stateless beyond its
// configuration fields; one Pipeline value is safe to reuse across
// many Run calls or to serve a single HTTP handler that calls
// Process per request.
//
// Construction is value-typed (no NewPipeline constructor) because
// every field has a sensible zero meaning: nil Sink panics on
// first Write (caller error), nil Extractors skips extraction,
// nil Logger silences error logs, RequireRedaction defaults to
// false BUT production callers always set it to true. Callers
// build a Pipeline literal at server-init time and reuse it.
type Pipeline struct {
	// Sink is required. Pipeline.Process / Run will panic if nil
	// — there's no graceful behaviour to fall back to.
	Sink Sink

	// Extractors, when non-nil, runs before each Sink.Write to
	// populate Event.Extractions. Skipped if Event.Extractions
	// is already populated (the Source pre-extracted) or if the
	// field is nil.
	Extractors *ExtractorRegistry

	// RequireRedaction enforces env.Redaction.Applied=true on
	// every event. Always true in production; tests that feed
	// synthetic envelopes set it false and trust their own
	// fixtures.
	RequireRedaction bool

	// Logger receives per-event errors during Run. nil silences
	// all logging — Run still counts errors in Stats.Errors so
	// callers can decide whether to fail.
	Logger *slog.Logger
}

// Process handles one already-parsed event. Used by the daemon's
// HTTP handler — a request becomes one Event and the surrounding
// Source/Sink ceremony of Run would be friction.
//
// Process is the single-event narrowing of Run; it shares the same
// validation, extraction, and Sink.Write call. Errors propagate
// directly to the caller (no per-event suppression — the daemon
// returns 5xx).
func (p *Pipeline) Process(ctx context.Context, e Event) (Result, error) {
	if e.Envelope == nil {
		return Result{}, errors.New("Pipeline.Process: nil envelope")
	}
	if p.RequireRedaction {
		if e.Envelope.Redaction == nil || !e.Envelope.Redaction.Applied {
			return Result{}, ErrRedactionRequired
		}
	}
	if p.Extractors != nil && len(e.Extractions) == 0 {
		e.Extractions = p.Extractors.Run(e.Envelope)
	}
	return p.Sink.Write(ctx, e)
}

// Run consumes from src, applies extractors, writes each event to
// the Sink, and calls Sink.Flush at the end. Per-event errors are
// counted in Stats.Errors and logged but do not abort the run —
// one bad envelope mid-import should not lose the rest. Context
// cancellation halts immediately and propagates the ctx error.
//
// The Sink is NOT closed by Run; the caller owns the Sink's
// lifecycle. Pattern:
//
//	sink := store.NewBufferedSink(s)
//	defer sink.Close()
//	stats, err := pipeline.Run(ctx, src)
func (p *Pipeline) Run(ctx context.Context, src Source) (Stats, error) {
	var stats Stats
	if src == nil {
		return stats, errors.New("Pipeline.Run: nil source")
	}
	for evt, err := range src.Events(ctx) {
		if err != nil {
			stats.Errors++
			if p.Logger != nil {
				p.Logger.Warn("source error", "err", err)
			}
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}
		result, err := p.Process(ctx, evt)
		if err != nil {
			stats.Errors++
			if p.Logger != nil {
				eventID := ""
				if evt.Envelope != nil {
					eventID = evt.Envelope.EventID
				}
				p.Logger.Warn("process error", "err", err, "event_id", eventID)
			}
			continue
		}
		stats.Processed++
		if result.Deduped {
			stats.Deduped++
		}
	}
	if err := p.Sink.Flush(ctx); err != nil {
		return stats, fmt.Errorf("sink flush: %w", err)
	}
	return stats, nil
}
