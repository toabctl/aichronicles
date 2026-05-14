// Package api implements the aichronicles-api HTTP daemon.
//
// One daemon, one process, one open SQLite handle. Reads, writes,
// SSE live activity, and the web HTML browser all share the same
// /v1/* mux; the hook subprocess (`aichronicles hook`) and every
// CLI subcommand are clients of this surface.
//
// Redaction is enforced server-side here via the Pipeline (see
// internal/events). Clients ship raw envelopes and trust the server to
// scrub before storage; even a buggy or malicious client claiming
// redaction.applied=true is re-redacted.
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// DefaultMaxEnvelopeBytes caps a single ingest body when the
// caller does not override. Real hook envelopes are usually tiny,
// but a single assistant_message carrying an inlined large tool
// result can legitimately exceed 16 MiB — we have observed real
// transcripts with ~49 MB single lines. 128 MiB is a sanity bound
// against pathological payloads; operators override via
// [limits].max_envelope_bytes in the config file.
const DefaultMaxEnvelopeBytes = 128 << 20

// HTTP timeout defaults for the api server. Conservative bounds
// against slow-read / slow-write attacks; 60s ReadTimeout is
// generous for a 128 MiB envelope over UDS (which is near-instant
// locally) and equally generous if the listener were ever exposed
// over a real network. WriteTimeout covers ack rendering; SSE
// connections override via http.ResponseController later.
const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 60 * time.Second
	httpWriteTimeout      = 30 * time.Second
	httpIdleTimeout       = 120 * time.Second
)

// DefaultIngestQueueMax is the rows-pending cap consulted when
// async ingest is enabled and the operator hasn't set
// [limits].ingest_queue_max. 10000 is generous for a personal-use
// deployment; if the queue sits at this depth steadily, the worker
// is hopelessly behind and 503-ing the next POST is more useful
// than silently buffering more.
const DefaultIngestQueueMax = 10000

// Server is the HTTP-facing surface of aichronicles-api. Owns the
// store handle, an events.Pipeline configured for server-side
// redaction, an in-process SSE bus that fan-outs every accepted
// ingest to live /v1/stream subscribers, and an IngestWorker that
// drains the ingest_pending staging table. Per-feature handlers
// live in handler_*.go and are methods on *Server.
//
// Two-phase ingest is unconditional: handleIngest enqueues the
// raw POST body in a tiny tx and returns 200 immediately; the
// worker drains pending rows on a background goroutine. The
// daemon (cmd/aichronicles-api) is responsible for spawning
// Worker().Run(ctx) so the worker's lifecycle is tied to the
// daemon's signal/context machinery and the shutdown order is
// correct (drain listener → cancel worker → wait for worker).
type Server struct {
	store            *store.Store
	slog             *slog.Logger
	maxEnvelopeBytes int
	pipeline         events.Pipeline
	sseBus           *sseBus
	worker           *IngestWorker
	ingestQueueMax   int
	// pendingDepth is the in-memory count of rows in ingest_pending.
	// Handler increments after a successful enqueue; worker
	// decrements after MarkPendingProcessed; backpressure reads
	// this instead of doing a SELECT COUNT(*) on every POST.
	// CAS-based reservation in handleIngest enforces the cap
	// without the TOCTOU race the previous CountPending-then-INSERT
	// pattern had.
	pendingDepth atomic.Int64
}

// NewServer returns a Server backed by s. log nil falls back to a
// stderr text handler. Pipeline is constructed with
// redact.Default() so the server is the single point of redaction
// enforcement; misconfiguration (nil scanner) would surface at
// first ingest as a 500 from the Pipeline's nil-Redactor guard.
//
// The IngestWorker is constructed but NOT started — the daemon
// main spawns Worker().Run(ctx) so the goroutine lifecycle is
// owned by the signal context and the shutdown order can be
// controlled. Tests can either start the worker themselves or
// call Worker().drain(ctx) explicitly after each POST for a
// deterministic single-step.
func NewServer(s *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	pipeline := events.Pipeline{
		Sink:       store.NewSink(s),
		Extractors: events.DefaultExtractors(),
		Redactor:   events.NewScannerRedactor(redact.Default()),
		Logger:     log,
	}
	bus := newSSEBus(log.With("component", "sse_bus"))
	srv := &Server{
		store:            s,
		slog:             log,
		maxEnvelopeBytes: DefaultMaxEnvelopeBytes,
		pipeline:         pipeline,
		sseBus:           bus,
		ingestQueueMax:   DefaultIngestQueueMax,
	}
	srv.worker = NewIngestWorker(s, pipeline, bus, log.With("component", "ingest_worker"), &srv.pendingDepth)
	// Seed pendingDepth from any rows the previous daemon
	// instance left in ingest_pending. Best-effort: a failure
	// here logs and leaves the counter at 0, which only matters
	// if there actually IS a leftover backlog AND the operator
	// is pushing enough load to hit the cap before the worker
	// drains it. We accept the brief over-acceptance window in
	// that pathological case; the alternative would be returning
	// an error from NewServer, which the daemon main can't
	// usefully recover from.
	if n, err := store.CountPending(context.Background(), s.DB()); err != nil {
		log.Warn("seed pendingDepth from store", "err", err)
	} else {
		srv.pendingDepth.Store(int64(n))
	}
	return srv
}

// Close terminates server-owned background subscribers (currently
// the SSE bus). Called by daemon main on shutdown so SSE
// goroutines exit before the listener closes.
func (s *Server) Close() {
	s.sseBus.Close()
}

// WithMaxEnvelopeBytes overrides the body cap. Returns the
// receiver for chaining; non-positive values are ignored
// (DefaultMaxEnvelopeBytes stays in effect). Designed for the
// daemon main to pipe cfg.Limits.MaxEnvelopeBytes through without
// mutating exported fields.
func (s *Server) WithMaxEnvelopeBytes(n int) *Server {
	if n > 0 {
		s.maxEnvelopeBytes = n
	}
	return s
}

// WithIngestQueueMax overrides the ingest_pending backlog cap.
// Non-positive values keep the built-in default (10000). Designed
// for the daemon main to pipe cfg.Limits.IngestQueueMax through
// without mutating exported fields. Returns the receiver for
// chaining alongside WithMaxEnvelopeBytes.
func (s *Server) WithIngestQueueMax(n int) *Server {
	if n > 0 {
		s.ingestQueueMax = n
	}
	return s
}

// Worker returns the IngestWorker NewServer constructed. The
// daemon main calls Worker().Run(ctx) in a goroutine so the
// worker's lifecycle is tied to the signal context's; tests can
// drive Worker().drain() directly for a deterministic step.
// Never nil.
func (s *Server) Worker() *IngestWorker { return s.worker }

// Handler returns the HTTP multiplexer with every /v1 route mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", s.handleIngest)
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/events", s.handleEventsList)
	mux.HandleFunc("GET /v1/sessions", s.handleSessionsList)
	mux.HandleFunc("GET /v1/sessions/resolve", s.handleSessionsResolve)
	mux.HandleFunc("GET /v1/sessions/source-agents", s.handleSessionsSourceAgents)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleSessionsGet)
	mux.HandleFunc("GET /v1/sessions/{id}/related", s.handleSessionsRelated)
	mux.HandleFunc("GET /v1/sessions/{id}/llm-outputs", s.handleSessionLLMOutputs)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handleSessionEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/extractions", s.handleSessionExtractions)
	mux.HandleFunc("GET /v1/sessions/{id}/candidate-priors", s.handleSessionCandidatePriors)
	mux.HandleFunc("GET /v1/sessions/{id}/outcome", s.handleSessionOutcome)
	mux.HandleFunc("GET /v1/sessions/{id}/start-cwd", s.handleSessionStartCwd)
	mux.HandleFunc("GET /v1/sessions/digests", s.handleSessionDigests)
	mux.HandleFunc("GET /v1/session-links", s.handleSessionLinks)
	mux.HandleFunc("GET /v1/llm-outputs/list", s.handleLLMOutputsList)
	mux.HandleFunc("GET /v1/llm-outputs/last-created-at", s.handleLLMOutputsLastCreated)
	mux.HandleFunc("GET /v1/llm-outputs/exists", s.handleLLMOutputExistsForSession)
	mux.HandleFunc("GET /v1/llm-outputs/{id}", s.handleLLMOutputByID)
	mux.HandleFunc("GET /v1/episodes", s.handleEpisodesList)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/facts/subjects", s.handleFactsSubjects)
	mux.HandleFunc("GET /v1/facts", s.handleFactsList)
	mux.HandleFunc("GET /v1/skills/staleness", s.handleSkillsStaleness)
	mux.HandleFunc("GET /v1/skills/impact", s.handleSkillsImpact)
	mux.HandleFunc("GET /v1/skills/invoked", s.handleSkillsInvoked)
	mux.HandleFunc("GET /v1/skills/installed", s.handleSkillsInstalled)
	mux.HandleFunc("GET /v1/audit", s.handleAudit)
	mux.HandleFunc("GET /v1/usage", s.handleUsage)
	mux.HandleFunc("GET /v1/skill-candidates", s.handleSkillCandidatesList)
	mux.HandleFunc("POST /v1/skill-candidates", s.handleSkillCandidatesRecord)
	mux.HandleFunc("POST /v1/skill-candidates/decision", s.handleSkillCandidatesDecision)
	mux.HandleFunc("GET /v1/skill-candidates/added", s.handleSkillCandidatesAdded)
	mux.HandleFunc("GET /v1/skill-candidates/effectiveness", s.handleSkillCandidatesEffectiveness)
	mux.HandleFunc("GET /v1/skill-candidates/pending", s.handleSkillCandidatesPending)
	mux.HandleFunc("POST /v1/skill-candidates/{id}/update", s.handleSkillCandidateUpdate)
	mux.HandleFunc("GET /v1/sessions/missing-summary", s.handleSessionsMissingSummary)
	mux.HandleFunc("GET /v1/sessions/needing-segmentation", s.handleSessionsNeedingSegmentation)
	mux.HandleFunc("GET /v1/sessions/completions", s.handleSessionsForCompletion)
	mux.HandleFunc("POST /v1/sessions/{id}/segment", s.handleSegmentSession)
	mux.HandleFunc("GET /v1/induction/candidates", s.handleInductionCandidates)
	mux.HandleFunc("GET /v1/proposals/failure-shapes", s.handleFailureShapes)
	mux.HandleFunc("GET /v1/skills/failures", s.handleSkillFailures)
	mux.HandleFunc("POST /v1/admin/vacuum", s.handleVacuum)
	mux.HandleFunc("GET /v1/admin/db-info", s.handleDBInfo)
	mux.HandleFunc("GET /v1/admin/stats", s.handleIngestStats)
	mux.HandleFunc("GET /v1/summaries", s.handleSummariesGet)
	mux.HandleFunc("GET /v1/summaries/batch", s.handleSummariesBatch)
	mux.HandleFunc("GET /v1/llm-outputs", s.handleLLMOutputGet)
	mux.HandleFunc("GET /v1/events/latest", s.handleEventsLatestBatch)
	mux.HandleFunc("GET /v1/unresolved", s.handleUnresolvedForCwd)
	mux.HandleFunc("GET /v1/projects/aggregates", s.handleProjectsAggregates)
	mux.HandleFunc("GET /v1/subagents", s.handleSubagentSpans)
	mux.HandleFunc("GET /v1/insights", s.handleInsights)
	mux.HandleFunc("POST /v1/llm-outputs", s.handleLLMOutputSave)
	mux.HandleFunc("POST /v1/episodes", s.handleEpisodesSave)
	mux.HandleFunc("POST /v1/facts", s.handleFactsSave)
	mux.HandleFunc("POST /v1/session-outcomes", s.handleSessionOutcomeSave)
	mux.HandleFunc("POST /v1/session-links", s.handleSessionLinksSave)
	mux.HandleFunc("POST /v1/import", s.handleImport)
	mux.HandleFunc("POST /v1/scrub", s.handleScrub)
	mux.HandleFunc("POST /v1/prune", s.handlePrune)
	mux.HandleFunc("GET /v1/stream", s.handleStream)
	return mux
}

// newHTTPServer builds an *http.Server with the timeouts every api
// listener should set. Centralised so ListenAndServe and the
// systemd-activated Serve path can't drift on the values.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

// runServer drives srv.Serve in a goroutine, logging non-shutdown
// errors via the caller's logger. http.ErrServerClosed is the
// expected return from a graceful Shutdown / Close and is silenced;
// anything else (a listener-level failure, an unexpected I/O
// error) is surfaced so an operator can act on it instead of
// staring at a silent socket.
//
// log must be non-nil — pass slog.Default() from a test if you
// don't have a project logger handy; the visible call-site
// argument keeps the dependency explicit instead of buried in a
// package-internal fallback.
func runServer(srv *http.Server, l net.Listener, log *slog.Logger) {
	go func() {
		err := srv.Serve(l)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api http server exited unexpectedly", "err", err)
		}
	}()
}

// ListenAndServe opens a Unix-domain listener at sockPath with 0600
// perms and serves until the returned shutdown func is invoked.
// The socket file is removed on shutdown.
//
// The returned shutdown function takes a context so the caller can
// bound graceful drain: in-flight requests run until they finish or
// the context fires, whichever comes first. A nil ctx is treated as
// "no drain" and is equivalent to a hard close.
func ListenAndServe(sockPath string, handler http.Handler, log *slog.Logger) (func(context.Context) error, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("ensure socket dir: %w", err)
	}
	// Remove any stale socket from a previous run. Safe because
	// UDS paths are owned by this process; a live server holding
	// the socket would have failed the MkdirAll already if there
	// was a permissions issue.
	_ = os.Remove(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", sockPath, err)
	}
	// Tighten the socket from the process default (typically 0644
	// with umask 0022) to 0600. The parent dir is already 0700 so
	// external users can't reach the file regardless of its mode,
	// which is why we don't go further (e.g. a process-wide umask
	// flip around Listen) — that would race with any other
	// goroutine doing file I/O at the same time, and the dir
	// perms already make the brief 0644 window unreachable.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	srv := newHTTPServer(handler)
	runServer(srv, l, log)

	shutdown := func(ctx context.Context) error {
		err := gracefulShutdown(ctx, srv)
		_ = os.Remove(sockPath)
		return err
	}
	return shutdown, nil
}

// gracefulShutdown runs srv.Shutdown(ctx) when a non-nil ctx is
// supplied and falls back to Close otherwise. Exposed as a helper
// so both ListenAndServe and Serve share the drain semantics.
func gracefulShutdown(ctx context.Context, srv *http.Server) error {
	if ctx == nil {
		return srv.Close()
	}
	return srv.Shutdown(ctx)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	maxBytes := int64(s.maxEnvelopeBytes)
	if maxBytes <= 0 {
		maxBytes = DefaultMaxEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Read request body failed", err.Error())
		return
	}
	if int64(len(body)) > maxBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "Envelope too large", "")
		return
	}

	var env events.Envelope
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed envelope JSON", err.Error())
		return
	}
	if err := env.Validate(); err != nil {
		writeProblem(w, http.StatusBadRequest, "Envelope validation failed", err.Error())
		return
	}

	// Two-phase ingest: write the raw POST body into the
	// ingest_pending staging table in a tiny tx, return 200
	// immediately. The IngestWorker drains pending rows on a
	// background goroutine — redact + extract + downstream insert
	// + FTS indexing + SSE publish all happen there, off the
	// hook's critical path.
	//
	// Backpressure: pendingDepth is a CAS-protected counter that
	// reflects the row count in ingest_pending (handler increments
	// on enqueue; worker decrements on MarkPendingProcessed; seeded
	// at NewServer from CountPending). The CAS loop reserves a slot
	// BEFORE the insert so two concurrent handlers can't both see
	// "depth=cap-1" and overshoot — the loser of the CAS sees the
	// new depth and either retries (if room appeared) or 503s.
	// The hook treats 503 (transport-like from its perspective) as
	// a drop and trips the outage path so the operator sees the
	// load explicitly rather than a silent buildup.
	maxDepth := int64(s.ingestQueueMax)
	for {
		cur := s.pendingDepth.Load()
		if cur >= maxDepth {
			s.slog.Warn("ingest: queue full — rejecting",
				"pending", cur, "max", maxDepth)
			writeProblem(w, http.StatusServiceUnavailable,
				"Ingest queue is full",
				fmt.Sprintf("pending=%d max=%d", cur, maxDepth))
			return
		}
		if s.pendingDepth.CompareAndSwap(cur, cur+1) {
			break
		}
	}

	var deduped bool
	if err := store.WithTx(r.Context(), s.store.DB(), func(tx *sql.Tx) error {
		var err error
		_, deduped, err = store.EnqueuePending(r.Context(), tx, env.EventID, body, time.Now().UnixMilli())
		return err
	}); err != nil {
		// Reservation didn't translate into a real row; release it.
		s.pendingDepth.Add(-1)
		s.slog.Error("ingest: enqueue", "event_id", env.EventID, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	// Dedup at phase 1: no new row was inserted (UNIQUE collision on
	// event_id), but we already reserved a slot. Release it so the
	// counter still reflects the actual row count in the table.
	if deduped {
		s.pendingDepth.Add(-1)
	}

	// Nudge the worker so it drains immediately rather than
	// waiting for the next heartbeat tick. Non-blocking: if a
	// wake is already queued this call is a no-op.
	s.worker.Wake()

	// Ack carries the in-flight envelope's identity (same
	// event_id, derived session id) so the hook's success path
	// is identical to what the sync pipeline used to return.
	// Deduped reflects ingest_pending's UNIQUE(event_id): a
	// retrying hook gets "yes I have this" without paying for
	// the pipeline a second time.
	writeJSON(w, http.StatusOK, events.Ack{
		EventID:   env.EventID,
		SessionID: events.DeriveSessionID(env.SourceAgent, env.SourceSessionID),
		Deduped:   deduped,
	})
}

// writeProblem renders an RFC 7807 problem+json response.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wire.Problem{Title: title, Status: status, Detail: detail})
}

// storeError is the canonical "store layer returned an error"
// response: log the op + err at ERROR level (so the operator sees
// the underlying SQL message in the daemon log) and surface a
// generic 500 to the client (so internal details don't leak across
// the wire). Every handler that calls store.Load*/store.Save*/etc.
// uses this — sixty-plus sites previously inlined the same three
// lines.
func (s *Server) storeError(w http.ResponseWriter, op string, err error) {
	s.slog.Error(op, "err", err)
	writeProblem(w, http.StatusInternalServerError, "Storage error", "")
}

// writeJSON renders a 2xx JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
