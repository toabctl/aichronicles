package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// handleScrub serves POST /v1/scrub. Body: wire.ScrubRequest.
// Response: wire.ScrubResponse. Always returns the report — the
// caller decides whether the rewrite count is acceptable.
//
// The transport-level concern: scrub holds SQLite's write lock
// for the duration of the scan, so a busy daemon will see hook
// ingests block until the scrub finishes. For very large stores
// the operator should run during quiet windows; the api accepts
// long requests because the http.Server's WriteTimeout is bounded
// only by the scan time once the response actually starts.
// clearWriteDeadlineForLongOp lifts the server-wide WriteTimeout for a
// handler whose work is unbounded by design.
//
// Scrub rescans every raw envelope, extraction and LLM output; Prune
// and Vacuum rewrite the file. On a multi-GB store those run for
// minutes, well past the 30s WriteTimeout — so the response write
// failed AFTER the work had already committed, and the operator saw an
// error for an operation that had actually succeeded. Worse, they
// would then likely run it again.
//
// Same technique the SSE handlers use, and safe for the same reason:
// these endpoints are reachable only over the 0600 UDS, so a
// deliberately slow client is not part of the threat model.
func clearWriteDeadlineForLongOp(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func (s *Server) handleScrub(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadlineForLongOp(w)
	defer func() { _ = r.Body.Close() }()

	var req wire.ScrubRequest
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProblem(w, http.StatusRequestEntityTooLarge,
				"Payload too large",
				fmt.Sprintf("body exceeds %d bytes", MaxJSONBodyBytes))
			return
		}
		writeProblem(w, http.StatusBadRequest, "Read request body failed", err.Error())
		return
	}
	if len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
			return
		}
	}

	report, err := store.Scrub(r.Context(), s.store.DB(), redact.Default(), store.ScrubOptions{
		DryRun: req.DryRun,
		// Out is intentionally nil: the api endpoint returns
		// the final report only. Operators that want streaming
		// per-row progress run `aichronicles scrub` locally,
		// which passes os.Stdout.
	})
	if err != nil {
		s.slog.Error("scrub", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Scrub failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, scrubReportToWire(report))
}

// handlePrune serves POST /v1/prune. Body: wire.PruneRequest.
// Response: wire.PruneResponse.
func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadlineForLongOp(w)
	defer func() { _ = r.Body.Close() }()

	var req wire.PruneRequest
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProblem(w, http.StatusRequestEntityTooLarge,
				"Payload too large",
				fmt.Sprintf("body exceeds %d bytes", MaxJSONBodyBytes))
			return
		}
		writeProblem(w, http.StatusBadRequest, "Read request body failed", err.Error())
		return
	}
	if len(body) == 0 {
		writeProblem(w, http.StatusBadRequest, "Empty body",
			"prune requires an explicit body with cutoff_ms")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed body", err.Error())
		return
	}
	if req.CutoffMs <= 0 {
		writeProblem(w, http.StatusBadRequest, "Invalid cutoff_ms",
			"must be > 0; the api will not delete with cutoff_ms=0 (would prune everything)")
		return
	}

	report, err := store.Prune(r.Context(), s.store.DB(), store.PruneOptions{
		CutoffMs:          req.CutoffMs,
		IncludeLLMOutputs: req.IncludeLLMOutputs,
		DryRun:            req.DryRun,
	})
	if err != nil {
		s.slog.Error("prune", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Prune failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.PruneResponse{
		Sessions:     report.Sessions,
		RawEnvelopes: report.RawEnvelopes,
		Events:       report.Events,
		Extractions:  report.Extractions,
		LLMOutputs:   report.LLMOutputs,
		DryRun:       report.DryRun,
		CutoffMs:     report.CutoffMs,
	})
}

func scrubReportToWire(r *store.ScrubReport) wire.ScrubResponse {
	if r == nil {
		return wire.ScrubResponse{}
	}
	return wire.ScrubResponse{
		EventsScanned:       r.EventsScanned,
		EventsRewritten:     r.EventsRewritten,
		EnvelopesRewritten:  r.EnvelopesRewritten,
		LLMOutputsScanned:   r.LLMOutputsScanned,
		LLMOutputsRewritten: r.LLMOutputsRewritten,

		ExtractionsScanned:   r.ExtractionsScanned,
		ExtractionsRewritten: r.ExtractionsRewritten,
		SessionsRederived:    r.SessionsRederived,

		PatternHits: r.PatternHits,
		DryRun:      r.DryRun,
	}
}
