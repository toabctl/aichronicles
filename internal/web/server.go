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
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// assetsFS holds every file under internal/web/assets/. Templates
// land under assets/templates/, vendored static files under
// assets/static/. `all:` walks dotfiles too in case future asset
// pipelines emit them.
//
//go:embed all:assets
var assetsFS embed.FS

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
	store     *store.Store
	cfg       Config
	log       *slog.Logger
	mux       *http.ServeMux
	pages     map[string]*template.Template
	fragments map[string]*template.Template

	// streamCount is the live count of /stream connections —
	// per-server (not package-global) so concurrent tests and
	// hypothetical multi-server setups don't share the cap.
	streamCount atomic.Int32
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
		store:     st,
		cfg:       cfg,
		log:       log,
		mux:       http.NewServeMux(),
		pages:     mustParsePages(),
		fragments: mustParseFragments(),
	}
	s.registerRoutes()
	return s
}

// pageNames is the canonical list of content templates the server
// renders. Each name maps to assets/templates/<name>.html, which
// is parsed alongside base.html into its own template set so the
// `content` block one page defines doesn't shadow another's.
var pageNames = []string{"sessions", "session", "search"}

// fragmentNames is the canonical list of htmx fragment templates
// the server renders. Each name maps to
// assets/templates/_<name>.html — the underscore prefix flags
// them as partials, parsed without base.html so they emit just
// the table or empty-state line that swaps into a target.
var fragmentNames = []string{"hits"}

// mustParsePages loads each page's template into a per-page
// template.Template, sharing only base.html. Single shared set
// would have one page's `{{define "content"}}` silently override
// another's — Go templates resolve by name across the whole set.
// Panics on parse error: fatal at startup, never at request time.
func mustParsePages() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).ParseFS(assetsFS,
			"assets/templates/base.html",
			"assets/templates/"+name+".html",
		)
		if err != nil {
			panic(fmt.Errorf("parse page %s: %w", name, err))
		}
		out[name] = t
	}
	return out
}

// mustParseFragments loads each fragment template (assets/
// templates/_<name>.html) into its own template set, with no
// base.html — fragments are HTML chunks htmx swaps into a
// target, not standalone documents.
func mustParseFragments() map[string]*template.Template {
	out := make(map[string]*template.Template, len(fragmentNames))
	for _, name := range fragmentNames {
		t, err := template.New(name).ParseFS(assetsFS,
			"assets/templates/_"+name+".html",
		)
		if err != nil {
			panic(fmt.Errorf("parse fragment %s: %w", name, err))
		}
		out[name] = t
	}
	return out
}

// registerRoutes wires every route handled by this server.
// Per-route handlers land in handlers.go, handler_session.go,
// and handler_search.go.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /{$}", s.sessionsHandler)
	s.mux.HandleFunc("GET /sessions/{id}", s.sessionDetailHandler)
	s.mux.HandleFunc("GET /search", s.searchHandler)
	s.mux.HandleFunc("GET /search/hits", s.searchHitsHandler)
	s.mux.HandleFunc("GET /stream", s.streamHandler)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS())))
}

// staticFS returns a sub-FS rooted at assets/static so paths like
// /static/pico.min.css resolve to the embedded file rather than
// requiring the prefix in the request URL.
func staticFS() fs.FS {
	sub, err := fs.Sub(assetsFS, "assets/static")
	if err != nil {
		// embed.FS guarantees this; an error means the embed
		// directive lost the directory at build time.
		panic(fmt.Errorf("static sub-fs: %w", err))
	}
	return sub
}

// render picks the page-specific template set and executes the
// "base" template against page data. Each set was loaded with
// base.html plus exactly one content page, so "base" resolves to
// the layout and {{template "content" .}} resolves to that
// page's content block.
func (s *Server) render(w http.ResponseWriter, _ *http.Request, name string, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("render: unknown page", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		s.log.Error("render template", "name", name, "err", err)
		// Don't double-write — ExecuteTemplate may have already
		// flushed headers before erroring.
	}
}

// renderFragment writes one htmx fragment template to w. Skips
// the base layout — htmx swaps the response into a target, so
// the response body should be just the fragment's HTML.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	t, ok := s.fragments[name]
	if !ok {
		s.log.Error("renderFragment: unknown fragment", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render fragment", "name", name, "err", err)
	}
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
		// BaseContext makes every request's r.Context() inherit
		// from Run's ctx — so when the user hits ctrl-C the
		// long-lived SSE handlers see r.Context().Done() fire
		// immediately. Without this, http.Server.Shutdown blocks
		// for the full ShutdownTimeout because Shutdown only
		// stops new connections; it doesn't cancel handlers.
		BaseContext: func(_ net.Listener) context.Context { return ctx },
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
