package web

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// sdListenFDsStart is the first file descriptor systemd passes via
// socket activation. Defined by the sd_listen_fds(3) convention.
const sdListenFDsStart = 3

// listenFromSystemd returns a net.Listener for the socket systemd
// activated us with, or (nil, nil) when socket activation is not in
// use. Callers fall back to opening their own listener in the latter
// case.
//
// Mirrors internal/daemon.ListenFromSystemd — duplicated rather than
// imported so the web package doesn't pull in the daemon's broader
// surface (ingest server, watchdog, etc.) for one helper. If a third
// caller appears, extract to a shared internal/sdsock package.
//
// systemd contract (see sd_listen_fds(3)):
//   - LISTEN_PID  = the PID that should read the fds; must match ours
//   - LISTEN_FDS  = number of fds passed, starting at fd 3
//   - LISTEN_FDNAMES (optional) = colon-separated names; ignored here
//
// We support exactly one listening fd; the unit file ships one
// ListenStream= directive, so anything else is a misconfiguration we
// surface as an error rather than silently ignore.
func listenFromSystemd() (net.Listener, error) {
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
