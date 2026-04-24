package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/toabctl/aichronicles/internal/ingest"
)

// Server implements the aichronicles ingest HTTP surface.
// It is transport-agnostic — wire it to a net.Listener of any kind.
type Server struct {
	logger *Logger
	slog   *slog.Logger
}

// NewServer returns a Server that persists accepted envelopes through logger.
// If log is nil, a default slog.Logger to stderr is used.
func NewServer(logger *Logger, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{logger: logger, slog: log}
}

// Handler returns the HTTP multiplexer with every /v1 route mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", s.handleIngest)
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	return mux
}

// ListenAndServe opens a Unix-domain listener at sockPath with 0600 perms
// and serves until the context is cancelled or the listener fails. The
// socket file is removed on shutdown.
func ListenAndServe(sockPath string, handler http.Handler) (func() error, error) {
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

	shutdown := func() error {
		err := srv.Close()
		_ = os.Remove(sockPath)
		return err
	}
	return shutdown, nil
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	var env ingest.Envelope
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		writeProblem(w, http.StatusBadRequest, "Malformed envelope JSON", err.Error())
		return
	}
	if err := env.Validate(); err != nil {
		writeProblem(w, http.StatusBadRequest, "Envelope validation failed", err.Error())
		return
	}

	env.SessionID = ingest.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	env.TsServer = time.Now().UTC()

	if err := s.logger.AppendJSON(env); err != nil {
		s.slog.Error("log append failed", "event_id", env.EventID, "err", err)
		writeProblem(w, http.StatusInternalServerError, "Log write failed", "")
		return
	}

	writeJSON(w, http.StatusOK, ingest.Ack{
		EventID:   env.EventID,
		SessionID: env.SessionID,
		Deduped:   false,
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
