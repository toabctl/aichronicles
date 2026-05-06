package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/events/sources/claude"
	"github.com/toabctl/aichronicles/internal/events/sources/gemini"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
)

// defaultHookTimeout caps the api round-trip so a wedged daemon
// can never block a Claude hook for long when [limits].ingest_timeout
// isn't set. 250ms is the CLAUDE.md hook-latency cap.
const defaultHookTimeout = 250 * time.Millisecond

// defaultHookAgent is the agent slug hook uses when invoked
// without --agent. Claude Code is the historical default and the
// hook command Claude's settings.json points at.
const defaultHookAgent = "claude-code"

// newHookCmd builds the cobra command for `aichronicles hook`.
// Renamed from `aichronicles ingest`: the subcommand previously
// targeted the legacy `aichroniclesd` daemon; after the api
// rearchitecture it talks to the unified `aichronicles-api`
// daemon over a different UDS.
func newHookCmd() *cobra.Command {
	var socketFlag string
	var agentSlug string
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Read a hook payload on stdin and forward as an envelope to aichronicles-api",
		Long: "hook is invoked by AI coding agent hooks (Claude Code by\n" +
			"default; pass --agent gemini-cli to consume Gemini CLI hook\n" +
			"payloads). It reads a JSON hook payload from stdin, wraps it\n" +
			"in the canonical Envelope, and POSTs to aichronicles-api over\n" +
			"a Unix socket. The api daemon applies redaction server-side.\n\n" +
			"Blocking policy: this command NEVER fails the hook. Errors are\n" +
			"logged to stderr as structured records and the process exits 0.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunHook(cmd.InOrStdin(), cmd.ErrOrStderr(), socketFlag, agentSlug)
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET; defaults to XDG_RUNTIME_DIR/aichronicles/api.sock)")
	cmd.Flags().StringVar(&agentSlug, "agent", defaultHookAgent, "source agent slug (claude-code | gemini-cli)")
	return cmd
}

// newHookLogger wraps the caller's stderr in a slog.Logger so every
// diagnostic emits a structured record. Tests pass a bytes.Buffer for
// stderr and assert on its contents.
func newHookLogger(stderr io.Writer) *slog.Logger {
	h := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With("cmd", "aichronicles hook")
}

// RunHook is the executable body of the hook subcommand, factored
// out so integration tests can drive it without forking a binary.
// It reads stdin, assembles an envelope, and forwards to
// aichronicles-api. The error return exists for the cobra interface;
// in practice this command always returns nil so a missing or broken
// daemon never fails a Claude hook.
func RunHook(stdin io.Reader, stderr io.Writer, socketFlag, agentSlug string) error {
	log := newHookLogger(stderr)

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
		agentSlug = defaultHookAgent
	}

	cfg, err := config.Load()
	if err != nil {
		log.Warn("config load failed, using defaults", "err", err)
		d := config.Default()
		cfg = &d
	}

	// Translate hook payload to canonical Envelope. Translators
	// are pure post-Phase 0; the api applies redaction
	// server-side.
	env, err := translateHook(agentSlug, raw)
	if err != nil {
		log.Error("translate hook payload", "agent", agentSlug, "err", err)
		return nil
	}

	// Coarse denylist: drop the whole envelope if its cwd falls
	// under a user-configured deny_paths entry. Runs after
	// translation so the cwd is canonical.
	if cfg.Capture.IsDenied(env.Cwd) {
		log.Info("envelope dropped by capture.deny_paths",
			"cwd", env.Cwd, "source_session_id", env.SourceSessionID)
		return nil
	}

	sockPath, err := paths.ResolveAPISocketPath(socketFlag)
	if err != nil {
		log.Error("resolve api socket path", "err", err)
		return nil
	}

	tracker := outageTracker(log)

	ctx, cancel := context.WithTimeout(context.Background(),
		cfg.Limits.IngestTimeout.Or(defaultHookTimeout))
	defer cancel()

	c := apiclient.NewClient(sockPath)
	if _, err := c.Ingest(ctx, env); err != nil {
		log.Error("post to aichronicles-api", "socket", sockPath, "err", err)
		// Daemon-outage signal: any transport-level failure
		// (socket missing, connection refused, timeout, EOF
		// mid-response) flips the outage flag. HTTP-status
		// errors (4xx/5xx with a problem body) do NOT — those
		// mean the daemon is up but rejected the envelope, and
		// treating them as "daemon unreachable" would produce
		// false positives on validation drift.
		var httpErr *apiclient.HTTPError
		if !errors.As(err, &httpErr) {
			maybeNotifyOutage(log, cfg, tracker, err)
		}
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
// internal/events/sources/. Translators are pure: they parse the hook
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
