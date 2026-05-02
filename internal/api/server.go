// Package api implements the aichronicles-api HTTP daemon.
//
// One daemon, one process, one open SQLite handle. Reads, writes,
// SSE live activity, and the web HTML browser all share the same
// /v1/* mux; the hook subprocess (`aichronicles hook`) and every
// CLI subcommand are clients of this surface.
//
// Redaction is enforced server-side here via the Pipeline (see
// pkg/events). Clients ship raw envelopes and trust the server to
// scrub before storage; even a buggy or malicious client claiming
// redaction.applied=true is re-redacted.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/api"
	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/redact"
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

// Server is the HTTP-facing surface of aichronicles-api. Owns the
// store handle and an events.Pipeline configured for server-side
// redaction. Per-feature handlers live in handler_*.go and are
// methods on *Server.
type Server struct {
	store            *store.Store
	slog             *slog.Logger
	maxEnvelopeBytes int
	pipeline         events.Pipeline
}

// NewServer returns a Server backed by s. log nil falls back to a
// stderr text handler. Pipeline is constructed with
// redact.Default() so the server is the single point of redaction
// enforcement; misconfiguration (nil scanner) would surface at
// first ingest as a 500 from the Pipeline's nil-Redactor guard.
func NewServer(s *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{
		store:            s,
		slog:             log,
		maxEnvelopeBytes: DefaultMaxEnvelopeBytes,
		pipeline: events.Pipeline{
			Sink:       store.NewSink(s),
			Extractors: events.DefaultExtractors(),
			Redactor:   events.NewScannerRedactor(redact.Default()),
			Logger:     log,
		},
	}
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

// Handler returns the HTTP multiplexer with every /v1 route mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", s.handleIngest)
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
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
// errors via slog.Default. http.ErrServerClosed is the expected
// return from a graceful Shutdown / Close and is silenced; anything
// else (a listener-level failure, an unexpected I/O error) is
// surfaced so an operator can act on it instead of staring at a
// silent socket.
func runServer(srv *http.Server, l net.Listener) {
	go func() {
		err := srv.Serve(l)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("api http server exited unexpectedly", "err", err)
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
func ListenAndServe(sockPath string, handler http.Handler) (func(context.Context) error, error) {
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
	runServer(srv, l)

	shutdown := func(ctx context.Context) error {
		err := gracefulShutdown(srv, ctx)
		_ = os.Remove(sockPath)
		return err
	}
	return shutdown, nil
}

// gracefulShutdown runs srv.Shutdown(ctx) when a non-nil ctx is
// supplied and falls back to Close otherwise. Exposed as a helper
// so both ListenAndServe and Serve share the drain semantics.
func gracefulShutdown(srv *http.Server, ctx context.Context) error {
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

	cap := int64(s.maxEnvelopeBytes)
	if cap <= 0 {
		cap = DefaultMaxEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, cap+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Read request body failed", err.Error())
		return
	}
	if int64(len(body)) > cap {
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

	// Pipeline.Process redacts (server-side, always — single point
	// of enforcement), runs the extractor registry, and writes
	// through the SQLite Sink in one transaction. Errors here are
	// programmer- or storage-side; clients can't influence them.
	result, err := s.pipeline.Process(r.Context(), events.Event{Envelope: &env, Raw: body})
	if err != nil {
		s.slog.Error("pipeline process", "event_id", env.EventID, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	writeJSON(w, http.StatusOK, events.Ack(result))
}

// writeProblem renders an RFC 7807 problem+json response.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Problem{Title: title, Status: status, Detail: detail})
}

// writeJSON renders a 2xx JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
