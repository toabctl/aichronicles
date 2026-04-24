package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/store"
)

// maxEnvelopeBytes caps the POST body we will read. Real envelopes
// are small, but the assistant_message content_text (last turn of a
// long session) can legitimately be large. 16MB is generous enough to
// accept anything realistic and tight enough to stop a pathological
// payload from blowing up the daemon.
const maxEnvelopeBytes = 16 << 20

// Server implements the aichronicles ingest HTTP surface backed by
// the SQLite store. Transport-agnostic — wire it to a net.Listener
// of any kind (UDS locally, HTTPS later).
type Server struct {
	store *store.Store
	slog  *slog.Logger
}

// NewServer returns a Server that persists accepted envelopes through
// the store. If log is nil, a default slog.Logger to stderr is used.
func NewServer(s *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{store: s, slog: log}
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

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = srv.Serve(l)
	}()

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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEnvelopeBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Read request body failed", err.Error())
		return
	}
	if int64(len(body)) > maxEnvelopeBytes {
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
