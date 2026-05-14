package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/toabctl/aichronicles/internal/apiclient"
)

// ErrDaemonRunning is returned by RefuseIfDaemonRunning when the api
// daemon's UDS is reachable. Maintenance subcommands (scrub,
// backfill) wrap it so the operator sees the canonical recovery
// instruction.
var ErrDaemonRunning = errors.New("aichronicles-api daemon is running — stop it before running this command")

// refuseIfDaemonRunningTimeout caps the healthz probe so the CLI
// never hangs longer than this when the socket is present but the
// daemon is wedged. Short on purpose — a healthy daemon responds
// in single-digit ms.
const refuseIfDaemonRunningTimeout = 2 * time.Second

// RefuseIfDaemonRunning returns ErrDaemonRunning whenever the api
// daemon's UDS is reachable, regardless of whether /v1/healthz is
// currently happy. Used by maintenance subcommands that touch
// ingest-path-owned tables (raw_envelopes, events, extractions)
// to prevent races with the daemon's IngestWorker.
//
// Returns nil in two "safe to proceed" cases:
//
//   - The socket file is absent (ErrSocketUnavailable): the daemon
//     was never started or has been cleanly shut down.
//   - The socket dial fails (no listener): same case, transient
//     between systemd cycles.
//
// Returns ErrDaemonRunning in all other cases — including a
// daemon reachable on the socket but answering non-2xx on
// /v1/healthz. An unhealthy daemon process may still be holding
// write locks, so a maintenance command racing it can corrupt the
// store; the caller MUST surface the error to the user and abort.
//
// sockFlag follows the same precedence as openAPIClient: explicit
// flag → $AICHRONICLES_API_SOCKET → XDG default.
//
// Resolution failure (the socket path itself can't be computed —
// XDG is unset, $AICHRONICLES_API_SOCKET points at an unreadable
// path, etc.) is surfaced as an error rather than silently
// proceeding. Previously a misconfigured env var made backfill /
// scrub assume "no daemon" even when one was running on a
// different socket the user had simply mistyped; aggressive
// correctness refuses to evaluate the premise rather than
// guessing.
func RefuseIfDaemonRunning(ctx context.Context, sockFlag string) error {
	c, err := openAPIClient(sockFlag)
	if err != nil {
		return fmt.Errorf("cannot verify daemon state (socket path resolution failed): %w; "+
			"pass --socket=<path> or fix $AICHRONICLES_API_SOCKET, "+
			"then re-run the command", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, refuseIfDaemonRunningTimeout)
	defer cancel()

	hErr := c.Healthz(probeCtx)
	switch {
	case hErr == nil:
		return fmt.Errorf("%w: stop aichronicles-api.service first", ErrDaemonRunning)
	case errors.Is(hErr, apiclient.ErrSocketUnavailable):
		// No socket = no daemon listening. Maintenance command
		// owns the store exclusively.
		return nil
	default:
		// Reachable but unhealthy or any other error — be
		// conservative and refuse: the daemon process may still
		// be holding write locks even if /v1/healthz is unhappy.
		return fmt.Errorf("%w: healthz probe returned %w", ErrDaemonRunning, hErr)
	}
}
