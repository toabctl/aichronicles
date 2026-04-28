package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
)

// sdListenFDsStart is the first file descriptor systemd passes via
// socket activation. Defined by the sd_listen_fds(3) convention.
const sdListenFDsStart = 3

// ListenFromSystemd returns a net.Listener for the socket systemd
// activated us with, or (nil, nil) when socket activation is not in
// use. Callers should fall back to opening their own listener in that
// case.
//
// systemd contract (see sd_listen_fds(3)):
//   - LISTEN_PID  = the PID that should read the fds; must match ours
//   - LISTEN_FDS  = number of fds passed, starting at fd 3
//   - LISTEN_FDNAMES (optional) = colon-separated names; ignored here
//
// We currently support exactly one listening fd; the unit file ships
// one ListenStream= directive, so anything else is a misconfiguration
// we surface as an error rather than silently ignore.
func ListenFromSystemd() (net.Listener, error) {
	fds := os.Getenv("LISTEN_FDS")
	if fds == "" || fds == "0" {
		return nil, nil
	}

	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil {
		return nil, fmt.Errorf("LISTEN_FDS set but LISTEN_PID missing/invalid: %w", err)
	}
	if pid != os.Getpid() {
		return nil, fmt.Errorf("LISTEN_PID=%d does not match our pid %d (socket inherited by wrong process?)", pid, os.Getpid())
	}

	n, err := strconv.Atoi(fds)
	if err != nil {
		return nil, fmt.Errorf("LISTEN_FDS has non-integer value %q: %w", fds, err)
	}
	if n != 1 {
		return nil, fmt.Errorf("expected exactly 1 socket from systemd, got LISTEN_FDS=%d", n)
	}

	f := os.NewFile(uintptr(sdListenFDsStart), "sd-listen")
	if f == nil {
		return nil, errors.New("failed to wrap fd 3 from systemd")
	}
	// net.FileListener dups the fd; we can close our File afterwards.
	l, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("wrap systemd fd as listener: %w", err)
	}
	return l, nil
}

// Serve runs the HTTP server on a caller-provided listener and returns
// a shutdown func that stops accepting new connections. Unlike
// ListenAndServe, it does not own the listener's backing socket — it
// will not chmod, unlink, or otherwise touch filesystem state. Use it
// when the listener came from systemd or elsewhere.
//
// The returned shutdown function takes a context so the caller can
// bound graceful drain: in-flight requests run until they finish or
// the context fires, whichever comes first. A nil ctx skips drain
// and hard-closes immediately.
func Serve(l net.Listener, handler http.Handler) func(context.Context) error {
	srv := newHTTPServer(handler)
	go func() { _ = srv.Serve(l) }()
	return func(ctx context.Context) error {
		return gracefulShutdown(srv, ctx)
	}
}
