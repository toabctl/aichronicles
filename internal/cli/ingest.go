package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/events/sources/claude"
	"github.com/toabctl/aichronicles/pkg/events/sources/gemini"
)

// defaultIngestTimeout caps the daemon round-trip so a wedged daemon
// can never block a Claude hook for long when [limits].ingest_timeout
// isn't set. 250ms is the CLAUDE.md hook-latency cap.
const defaultIngestTimeout = 250 * time.Millisecond

// defaultIngestAgent is the agent slug ingest uses when invoked
// without --agent. Claude Code is the historical default and the
// hook command Claude's settings.json points at; existing
// installs keep working without flag changes.
const defaultIngestAgent = "claude-code"

func newIngestCmd() *cobra.Command {
	var socketFlag string
	var agentSlug string
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Read a hook payload on stdin and forward as an envelope",
		Long: "ingest is invoked by AI coding agent hooks (Claude Code by\n" +
			"default; pass --agent codex to consume OpenAI Codex CLI hook\n" +
			"payloads). It reads a JSON hook payload from stdin, wraps it\n" +
			"in the canonical Envelope, and POSTs to aichroniclesd over a\n" +
			"Unix socket.\n\n" +
			"Blocking policy: this command NEVER fails the hook. Errors are\n" +
			"logged to stderr as structured records and the process exits 0.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunIngest(cmd.InOrStdin(), cmd.ErrOrStderr(), socketFlag, agentSlug)
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "", "daemon UDS path (overrides $AICHRONICLES_SOCKET; defaults to XDG_RUNTIME_DIR)")
	cmd.Flags().StringVar(&agentSlug, "agent", defaultIngestAgent, "source agent slug (claude-code | gemini-cli)")
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
func RunIngest(stdin io.Reader, stderr io.Writer, socketFlag, agentSlug string) error {
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

	if agentSlug == "" {
		agentSlug = defaultIngestAgent
	}

	cfg, err := config.Load()
	if err != nil {
		log.Warn("config load failed, using defaults", "err", err)
		d := config.Default()
		cfg = &d
	}

	// Translate hook payload to canonical Envelope. Each source's
	// HookTranslator owns redaction (the source IS the edge), so by
	// the time Translate returns, env.Redaction.Applied is true.
	env, err := translateHook(agentSlug, raw)
	if err != nil {
		log.Error("translate hook payload", "agent", agentSlug, "err", err)
		return nil
	}

	// Coarse denylist: drop the whole envelope if its cwd falls under
	// a user-configured deny_paths entry. Runs after translation so
	// the cwd is canonical, but the envelope is already redacted —
	// nothing useful is logged here either way.
	if cfg.Capture.IsDenied(env.Cwd) {
		log.Info("envelope dropped by capture.deny_paths",
			"cwd", env.Cwd, "source_session_id", env.SourceSessionID)
		return nil
	}

	sockPath, err := paths.ResolveSocketPath(socketFlag)
	if err != nil {
		log.Error("resolve socket path", "err", err)
		return nil
	}

	tracker := outageTracker(log)

	ctx, cancel := context.WithTimeout(context.Background(),
		cfg.Limits.IngestTimeout.Or(defaultIngestTimeout))
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

// translateHook dispatches to the per-agent HookTranslator under
// pkg/events/sources/. Translators are pure: they parse the hook
// payload and produce a canonical Envelope. Redaction is applied
// server-side by the receiving daemon's events.Pipeline. Unknown
// agent slugs return an error so a typo in --agent surfaces
// immediately rather than producing a malformed envelope.
func translateHook(agentSlug string, raw []byte) (events.Envelope, error) {
	now := func() time.Time { return time.Now().UTC() }
	switch agentSlug {
	case "claude-code":
		tr := &claude.HookTranslator{Now: now}
		return tr.Translate(raw)
	case "gemini-cli":
		tr := &gemini.HookTranslator{Now: now}
		return tr.Translate(raw)
	default:
		return events.Envelope{}, fmt.Errorf("unknown agent slug %q", agentSlug)
	}
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
