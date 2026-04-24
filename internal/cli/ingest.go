package cli

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/redact"
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
			"logged to stderr as structured records and the process exits 0.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunIngest(cmd.InOrStdin(), cmd.ErrOrStderr(), socketFlag)
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "", "daemon UDS path (default: $XDG_RUNTIME_DIR/aichronicles/sock)")
	return cmd
}

// newIngestLogger wraps the caller's stderr in a slog.Logger so every
// diagnostic emits a structured record (time, level, cmd, message, and
// any supplied attributes). Tests pass a bytes.Buffer for stderr and
// assert on its contents, so they observe the same structured output
// the user sees in a real hook invocation.
func newIngestLogger(stderr io.Writer) *slog.Logger {
	h := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With("cmd", "aichronicles ingest")
}

// RunIngest is the executable body of the ingest subcommand, factored out
// so integration tests can drive it without forking a binary. It reads
// stdin, assembles an envelope, and forwards to the daemon. The error
// return exists for the cobra interface; in practice this command always
// returns nil so a missing or broken daemon never fails a Claude hook.
func RunIngest(stdin io.Reader, stderr io.Writer, socketFlag string) error {
	log := newIngestLogger(stderr)

	raw, err := io.ReadAll(stdin)
	if err != nil {
		log.Error("read stdin", "err", err)
		return nil
	}
	if len(raw) == 0 {
		log.Warn("empty stdin; nothing to ingest")
		return nil
	}

	env, err := Assemble(raw, time.Now().UTC())
	if err != nil {
		log.Error("assemble envelope", "err", err)
		return nil
	}

	// Redact at the edge: secrets present in the original hook payload
	// must never leave this process unscrubbed. Downstream — daemon,
	// store, future LLM shim — treats Redaction.Applied as proof that
	// this step ran.
	ingest.ApplyRedaction(&env, redact.Default())

	sockPath := socketFlag
	if sockPath == "" {
		sockPath, err = paths.Socket()
		if err != nil {
			log.Error("resolve socket path", "err", err)
			return nil
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Warn("config load failed, using defaults", "err", err)
		d := config.Default()
		cfg = &d
	}

	tracker := outageTracker(log)

	ctx, cancel := context.WithTimeout(context.Background(), ingestTimeout)
	defer cancel()

	client := NewClient(sockPath)
	if _, err := client.Post(ctx, env); err != nil {
		log.Error("post to daemon", "socket", sockPath, "err", err)
		maybeNotifyOutage(log, cfg, tracker, err)
		return nil
	}

	// Post succeeded — drop any lingering outage flag so the next
	// failure gets its own notification.
	if tracker != nil {
		if err := tracker.Clear(); err != nil {
			log.Warn("clear outage flag", "err", err)
		}
	}
	return nil
}

// outageTracker resolves the outage flag path and returns a tracker, or
// nil when the path cannot be built (in which case notifications simply
// fire every time — acceptable degradation, never block the hook).
func outageTracker(log *slog.Logger) *notify.OutageTracker {
	path, err := paths.OutageFlag()
	if err != nil {
		log.Warn("resolve outage flag path", "err", err)
		return nil
	}
	return notify.NewOutageTracker(path)
}

// maybeNotifyOutage sends one desktop notification per outage when the
// user has opted in. Swallows all errors — no notification must ever
// fail a hook.
func maybeNotifyOutage(log *slog.Logger, cfg *config.Config, tracker *notify.OutageTracker, cause error) {
	if !cfg.Notifications.DaemonUnreachable || tracker == nil || !tracker.ShouldNotify() {
		return
	}
	if err := notify.New(true).Send(
		"aichronicles: daemon unreachable",
		"hooks are losing events: "+cause.Error(),
	); err != nil {
		log.Warn("notify failed", "err", err)
		return
	}
	if err := tracker.MarkNotified(); err != nil {
		log.Warn("mark outage notified", "err", err)
	}
}
