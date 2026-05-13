package events

import (
	"context"
	"encoding/json"
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
// nil Logger silences error logs. Redactor is required in
// production: a Pipeline with nil Redactor returns
// ErrRedactionRequired from Process — fail loud rather than
// silently storing unredacted bytes.
type Pipeline struct {
	// Sink is required. Pipeline.Process / Run will panic if nil
	// — there's no graceful behaviour to fall back to.
	Sink Sink

	// Extractors, when non-nil, runs before each Sink.Write to
	// populate Event.Extractions. Skipped if Event.Extractions
	// is already populated (the Source pre-extracted) or if the
	// field is nil.
	Extractors *ExtractorRegistry

	// Redactor scrubs the envelope in place before extractor
	// dispatch and Sink.Write. The Pipeline is the single point
	// of redaction enforcement: Sources ship raw envelopes and
	// trust the Pipeline to scrub. Redactor.Apply sets
	// env.Redaction.Applied=true, and the Pipeline re-marshals
	// e.Raw with the post-redaction Envelope so the Sink stores
	// scrubbed bytes.
	//
	// Required. A nil Redactor causes Process to return
	// ErrRedactionRequired without calling the Sink — that is
	// safer than the alternative ("forgot to wire a Redactor →
	// secrets land in storage"). Tests that genuinely want a
	// no-op Redactor pass NewScannerRedactor(nil).
	Redactor Redactor

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
//
// Perf note (arch_review_2026_05_14 #7, deferred): Process is
// strictly serial today. Redaction + extraction + Sink.Write run
// sequentially per envelope and the IngestWorker calls this one
// row at a time. A multi-MB envelope can consume seconds of
// per-row latency — which is why workerShutdownGrace had to be
// raised to 20s and why the shutdown drain has a fresh budget
// separate from the listener drain. The remediation is bounded
// parallel extraction (extractors are independent and pure;
// redaction reuses the Scanner; Sink.Write is the only ordering
// point and FTS5's serialized writer is the lower bound). Out of
// scope for the arch-review pass; needs its own perf-focused
// branch with benchmarks and FTS5 contention measurement before
// changing the shape.
func (p *Pipeline) Process(ctx context.Context, e Event) (Result, error) {
	if e.Envelope == nil {
		return Result{}, errors.New("Pipeline.Process: nil envelope")
	}
	if p.Redactor == nil {
		// Fail loud rather than silently store unredacted bytes.
		// This is a programmer error — production wiring always
		// supplies a Redactor; tests that genuinely want a no-op
		// pass NewScannerRedactor(nil).
		return Result{}, ErrRedactionRequired
	}
	p.Redactor.Apply(e.Envelope)
	// After redaction, the canonical bytes for raw_envelopes are
	// the re-marshaled post-redaction Envelope. The original
	// e.Raw came from the wire (POST body) or transcript line
	// before the server scrubbed it; storing those bytes would
	// re-introduce the secret we just removed. Re-marshal once
	// here so every Sink sees redacted Raw automatically.
	marshaled, err := json.Marshal(e.Envelope)
	if err != nil {
		return Result{}, fmt.Errorf("Pipeline.Process: marshal post-redaction envelope: %w", err)
	}
	e.Raw = marshaled

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
//	defer sink.Close(ctx)
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
		if _, err := p.Process(ctx, evt); err != nil {
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
	}
	if err := p.Sink.Flush(ctx); err != nil {
		return stats, fmt.Errorf("sink flush: %w", err)
	}
	// Read final committed counts from the Sink. This is the only
	// reliable path for buffered sinks where per-Write Result is
	// synthetic pre-flush. Single-tx sinks are also fine here
	// because they update on every Write.
	sinkStats := p.Sink.Stats()
	stats.Processed = sinkStats.Imported + sinkStats.Deduped
	stats.Deduped = sinkStats.Deduped
	return stats, nil
}
