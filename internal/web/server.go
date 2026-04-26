// Package web hosts the aichronicles web UI — a small read-only
// HTTP server that lists sessions, surfaces cached LLM summaries,
// and exposes the same FTS5 search the CLI and MCP server use.
//
// Like `aichronicles mcp-serve`, the web server is a short-lived
// CLI subprocess (not part of `aichroniclesd`): it opens the
// SQLite store directly in read mode, never writes, never proxies
// through the daemon's UDS. SQLite WAL handles the read/write
// concurrency between us and the daemon. The daemon's UDS is the
// write path; the web server is a read path. Same boundary the
// MCP server already uses.
//
// Default bind is 127.0.0.1 — the localhost-only boundary is the
// auth model, mirroring the daemon's 0600 UDS. Binding to a
// public address is opt-in (--bind) and surfaces a startup
// warning.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// DefaultPort is the loopback port aichronicles web binds to by
// default. Picked to avoid collisions with the common range
// (3000/4000/5000/8000/8080/9000) without claiming a registered
// IANA service.
const DefaultPort = 7878

// DefaultBind is the loopback address. Configurable via flag for
// users who want LAN access; the cmd-layer prints a warning when
// the bind isn't a loopback address.
const DefaultBind = "127.0.0.1"

// Config is what NewServer needs. Zero values fall back to
// DefaultBind / DefaultPort so tests can pass an empty Config and
// drive the server on an ephemeral port via httptest.
type Config struct {
	// Bind is the listen address (e.g. "127.0.0.1", "::1",
	// "0.0.0.0"). Empty == DefaultBind.
	Bind string

	// Port is the listen port. Zero == DefaultPort. Pass 0
	// explicitly via Listener (below) for an ephemeral port.
	Port int

	// Listener, when non-nil, overrides Bind/Port — the server
	// uses this listener instead of opening a new one. Lets tests
	// run on an ephemeral port returned by httptest.
	Listener net.Listener

	// ShutdownTimeout caps how long we wait for in-flight requests
	// after Run's ctx is cancelled. 5 s is plenty for the read
	// paths this server exposes.
	ShutdownTimeout time.Duration
}

// Server is one running web instance. Constructed via NewServer,
// kicked off via Run. Safe to call Run concurrently with multiple
// requests, not safe to call Run twice.
type Server struct {
	store *store.Store
	cfg   Config
	log   *slog.Logger
	mux   *http.ServeMux
}

// NewServer wires the routes against st. The caller retains
// ownership of st — closing it is the caller's job. log is used
// for startup, shutdown, and per-request access lines; pass
// slog.Default() if you don't have a project logger handy.
func NewServer(st *store.Store, cfg Config, log *slog.Logger) *Server {
	if cfg.Bind == "" {
		cfg.Bind = DefaultBind
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		store: st,
		cfg:   cfg,
		log:   log,
		mux:   http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// registerRoutes wires every route handled by this server.
// Per-route handlers land in their own files as they're added.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.healthz)
}

// Addr returns the address the server is configured to listen on.
// Useful for tests that didn't supply a Listener and want to know
// where to point a client.
func (s *Server) Addr() string {
	if s.cfg.Listener != nil {
		return s.cfg.Listener.Addr().String()
	}
	return fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.Port)
}

// Run starts the HTTP server and blocks until ctx is cancelled or
// the server errors out. ctx cancellation triggers a graceful
// shutdown bounded by Config.ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler: s.mux,
		// Read/Write timeouts protect against slow-loris-style
		// hangs even on a localhost service.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln := s.cfg.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.Addr())
		if err != nil {
			return fmt.Errorf("listen %s: %w", s.Addr(), err)
		}
	}

	s.log.Info("aichronicles web listening",
		"addr", ln.Addr().String(),
		"public", isPublicBind(s.cfg.Bind))

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	case err := <-serveErr:
		return err
	}
}

// healthz is a trivial liveness check. Unauthenticated and
// always-on — handy for `curl http://127.0.0.1:7878/healthz` to
// confirm the server is up without rendering a template.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// isPublicBind reports whether the configured Bind address would
// expose the server outside loopback. Used by the cmd layer to
// emit a startup warning, not by the server itself.
func isPublicBind(bind string) bool {
	if bind == "" || bind == "127.0.0.1" || bind == "::1" || bind == "localhost" {
		return false
	}
	// "0.0.0.0", "::", or any non-loopback IP/hostname counts as
	// public. Conservative — false positives are fine here, they
	// produce a warning, not a refusal.
	return !strings.HasPrefix(bind, "127.")
}
