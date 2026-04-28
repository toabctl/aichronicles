package daemon

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
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// DefaultMaxEnvelopeBytes is the body cap NewServer applies when the
// caller doesn't override it. Real hook envelopes are usually tiny,
// but a single assistant_message carrying an inlined large tool
// result can legitimately exceed 16 MiB — we have observed real
// transcripts with ~49 MB single lines. Keep this in lockstep with
// maxClaudeLineBytes in cli/import_claude.go so the live-hook path
// and the backfill path accept the same shape of envelope. 128 MiB
// is a sanity bound against a pathological payload; operators can
// override via [limits].max_envelope_bytes in the config file (the
// daemon main wires it through to NewServer).
const DefaultMaxEnvelopeBytes = 128 << 20

// HTTP timeout defaults for the ingest server. Conservative bounds
// against slow-read / slow-write attacks; 60s ReadTimeout is
// generous for a 128 MiB envelope over UDS (which is near-instant
// locally) and equally generous if the listener is ever exposed
// over a real network. WriteTimeout covers ack rendering, which is
// always a small JSON body. IdleTimeout bounds keep-alive holding.
const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 60 * time.Second
	httpWriteTimeout      = 30 * time.Second
	httpIdleTimeout       = 120 * time.Second
)

// newHTTPServer builds an *http.Server with the timeouts every
// daemon listener should set. Centralised so ListenAndServe and the
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

// runServer drives srv.Serve in a goroutine, logging any non-shutdown
// error via slog.Default. http.ErrServerClosed is the expected return
// from a graceful Shutdown / Close and is silenced; anything else
// (a listener-level failure, an unexpected I/O error) is surfaced so
// an operator can act on it instead of staring at a silent socket.
func runServer(srv *http.Server, l net.Listener) {
	go func() {
		err := srv.Serve(l)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("http server exited unexpectedly", "err", err)
		}
	}()
}

// Server implements the aichronicles ingest HTTP surface backed by
// the SQLite store. Transport-agnostic — wire it to a net.Listener
// of any kind (UDS locally, HTTPS later).
type Server struct {
	store            *store.Store
	slog             *slog.Logger
	maxEnvelopeBytes int
}

// NewServer returns a Server that persists accepted envelopes through
// the store. If log is nil, a default slog.Logger to stderr is used.
// If maxEnvelopeBytes <= 0, DefaultMaxEnvelopeBytes is used.
func NewServer(s *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{store: s, slog: log, maxEnvelopeBytes: DefaultMaxEnvelopeBytes}
}

// WithMaxEnvelopeBytes overrides the body cap on the receiver.
// Returns the receiver for chaining; non-positive values are
// ignored (DefaultMaxEnvelopeBytes stays in effect). Designed for
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

// ListenAndServe opens a Unix-domain listener at sockPath with 0600 perms
// and serves until the returned shutdown func is invoked. The socket
// file is removed on shutdown.
//
// The returned shutdown function takes a context so the caller can
// bound graceful drain: in-flight requests run until they finish or
// the context fires, whichever comes first. A nil ctx is treated as
// "no drain" and is equivalent to a hard close.
func ListenAndServe(sockPath string, handler http.Handler) (func(context.Context) error, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("ensure socket dir: %w", err)
	}
	// Remove any stale socket from a previous run. Safe because UDS paths
	// are owned by this process; a live server holding the socket would
	// have failed the MkdirAll already if there was a permissions issue.
	_ = os.Remove(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", sockPath, err)
	}
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
// supplied and falls back to Close otherwise. Exposed as a helper so
// both ListenAndServe and Serve share the drain semantics.
func gracefulShutdown(srv *http.Server, ctx context.Context) error {
	if ctx == nil {
		return srv.Close()
	}
	return srv.Shutdown(ctx)
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// Buffer the body so we can strict-decode AND also store the
	// original bytes verbatim in raw_envelopes.envelope_json. io.ReadAll
	// is fine: our limit bounds memory.
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

	var env ingest.Envelope
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

	// Defense-in-depth: the client CLI is responsible for redaction,
	// but a forgetful or third-party client might skip it. Refuse to
	// persist anything that hasn't been explicitly marked as scrubbed.
	// We never silently re-scrub server-side — that would hide a
	// broken client from operator view.
	if env.Redaction == nil || !env.Redaction.Applied {
		s.slog.Warn("rejecting unredacted envelope",
			"event_id", env.EventID,
			"source_agent", env.SourceAgent,
		)
		writeProblem(w, http.StatusBadRequest,
			"Redaction required",
			"envelope.redaction.applied must be true; run the client's redactor before POSTing")
		return
	}

	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		s.slog.Error("begin tx", "event_id", env.EventID, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	defer func() { _ = tx.Rollback() }()

	tsServer := time.Now().UTC().UnixMilli()
	deduped, err := store.IngestEnvelope(r.Context(), tx, &env, body, tsServer)
	if err != nil {
		s.slog.Error("store.IngestEnvelope", "event_id", env.EventID, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}
	if err := tx.Commit(); err != nil {
		s.slog.Error("commit", "event_id", env.EventID, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Storage error", "")
		return
	}

	writeJSON(w, http.StatusOK, ingest.Ack{
		EventID:   env.EventID,
		SessionID: store.ResolveSessionID(env.SourceAgent, env.SourceSessionID),
		Deduped:   deduped,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Problem follows RFC 7807 shape. Served as application/problem+json.
type Problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Title: title, Status: status, Detail: detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
