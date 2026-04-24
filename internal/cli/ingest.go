package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
)

// ingestTimeout caps the daemon round-trip so a wedged daemon can never
// block a Claude hook for long. 250ms is the CLAUDE.md hook-latency cap.
const ingestTimeout = 250 * time.Millisecond

func newIngestCmd() *cobra.Command {
	var socketFlag string
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Read a hook payload on stdin and forward as an envelope",
		Long: "ingest is invoked by Claude Code hooks. It reads a JSON hook\n" +
			"payload from stdin, wraps it in the canonical Envelope, and POSTs\n" +
			"to aichroniclesd over a Unix socket.\n\n" +
			"Blocking policy: this command NEVER fails the hook. Errors are\n" +
			"logged to stderr and the process exits 0. That way a crashed or\n" +
			"missing daemon never blocks the Claude session.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunIngest(cmd.InOrStdin(), cmd.ErrOrStderr(), socketFlag)
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "", "daemon UDS path (default: $XDG_RUNTIME_DIR/aichronicles/sock)")
	return cmd
}

// RunIngest is the executable body of the ingest subcommand, factored out
// so integration tests can drive it without forking a binary. It reads
// stdin, assembles an envelope, and forwards to the daemon. The error
// return exists for the cobra interface; in practice this command always
// returns nil so a missing or broken daemon never fails a Claude hook.
func RunIngest(stdin io.Reader, stderr io.Writer, socketFlag string) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		warn(stderr, "read stdin:", err)
		return nil
	}
	if len(raw) == 0 {
		warn(stderr, "empty stdin; nothing to ingest")
		return nil
	}

	env, err := Assemble(raw, time.Now().UTC())
	if err != nil {
		warn(stderr, "assemble:", err)
		return nil
	}

	sockPath := socketFlag
	if sockPath == "" {
		sockPath, err = paths.Socket()
		if err != nil {
			warn(stderr, "resolve socket:", err)
			return nil
		}
	}

	cfg, err := config.Load()
	if err != nil {
		warn(stderr, "config load failed, using defaults:", err)
		d := config.Default()
		cfg = &d
	}

	tracker := outageTracker(stderr)

	ctx, cancel := context.WithTimeout(context.Background(), ingestTimeout)
	defer cancel()

	client := NewClient(sockPath)
	if _, err := client.Post(ctx, env); err != nil {
		warn(stderr, err)
		maybeNotifyOutage(stderr, cfg, tracker, err)
		return nil
	}

	// Post succeeded — if a prior outage flag lingered, drop it so the
	// next failure gets its own notification.
	if tracker != nil {
		if err := tracker.Clear(); err != nil {
			warn(stderr, "clear outage flag:", err)
		}
	}
	return nil
}

// outageTracker resolves the outage flag path and returns a tracker, or
// nil when we cannot build one (in which case notifications simply fire
// every time — acceptable degradation, never block the hook).
func outageTracker(stderr io.Writer) *notify.OutageTracker {
	path, err := paths.OutageFlag()
	if err != nil {
		warn(stderr, "resolve outage flag:", err)
		return nil
	}
	return notify.NewOutageTracker(path)
}

// maybeNotifyOutage sends one desktop notification per outage when the
// user has opted in. Swallows all errors — no notification must ever
// fail a hook.
func maybeNotifyOutage(stderr io.Writer, cfg *config.Config, tracker *notify.OutageTracker, cause error) {
	if !cfg.Notifications.DaemonUnreachable || tracker == nil || !tracker.ShouldNotify() {
		return
	}
	if err := notify.New(true).Send(
		"aichronicles: daemon unreachable",
		fmt.Sprintf("hooks are losing events. %v", cause),
	); err != nil {
		warn(stderr, "notify failed:", err)
		return
	}
	if err := tracker.MarkNotified(); err != nil {
		warn(stderr, "mark outage notified:", err)
	}
}

// warn writes a single-line diagnostic prefixed with the CLI name to w.
// The Fprintln error return is intentionally discarded — there is nothing
// useful we could do if writing to stderr itself fails.
func warn(w io.Writer, args ...any) {
	parts := append([]any{"aichronicles ingest:"}, args...)
	_, _ = fmt.Fprintln(w, parts...)
}
