package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/events/sources/claude"
	"github.com/toabctl/aichronicles/internal/events/sources/codex"
	"github.com/toabctl/aichronicles/internal/events/sources/gemini"
	"github.com/toabctl/aichronicles/internal/notify"
	"github.com/toabctl/aichronicles/internal/paths"
)

// defaultHookTimeout caps the api round-trip so a wedged daemon
// can never block a Claude hook for long when [limits].ingest_timeout
// isn't set.
//
// History: 250ms → 2s → 5s. The 250ms default was sized for a sub-
// millisecond UDS POST + a tiny SQLite insert; the daemon's
// synchronous pipeline outgrew it and large hooks silently lost
// events. The 2s stopgap was sized for a healthy box; under
// real-world CPU/IO contention (e.g. a parallel build, a heavy LLM
// turn, a backed-up disk) the daemon's body-read + CAS + tiny tx
// + commit can blow past 2s, the client cancels, and the hook
// emits event_dropped — even though, since the 6bd20e5 fix, the
// daemon's enqueue runs on its own decoupled ingestEnqueueBudget
// (10s) and the row almost always still lands. 5s aligns the
// CLI-side deadline closer to that internal budget so loaded-box
// drops stop showing up as phantom outage notifications, while
// still capping the worst-case hook stall in front of the user.
//
// Operators on slower machines can override further via
// [limits].ingest_timeout.
const defaultHookTimeout = 5 * time.Second

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
			"default; pass --agent gemini-cli or --agent codex-cli to\n" +
			"consume Gemini CLI / Codex CLI hook payloads). It reads a\n" +
			"JSON hook payload from stdin, wraps it in the canonical\n" +
			"Envelope, and POSTs to aichronicles-api over\n" +
			"a Unix socket. The api daemon applies redaction server-side.\n\n" +
			"Blocking policy: this command NEVER fails the hook. Errors are\n" +
			"logged to stderr as structured records and the process exits 0.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunHook(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), socketFlag, agentSlug)
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET; defaults to XDG_RUNTIME_DIR/aichronicles/api.sock)")
	cmd.Flags().StringVar(&agentSlug, "agent", defaultHookAgent, "source agent slug (claude-code | gemini-cli | codex-cli)")
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
//
// ctx is the parent for the per-request ingest deadline so Ctrl-C
// (cobra installs signal-handling on the root command's context)
// cancels the in-flight POST instead of waiting out the full
// IngestTimeout. Tests that don't need cancellation pass
// context.Background().
func RunHook(ctx context.Context, stdin io.Reader, stderr io.Writer, socketFlag, agentSlug string) error {
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

	parentCtx := ctx
	budget := cfg.Limits.IngestTimeout.Or(defaultHookTimeout)
	c := apiclient.NewClient(sockPath)
	postOnce := func() error {
		attemptCtx, cancel := context.WithTimeout(parentCtx, budget)
		defer cancel()
		_, err := c.Ingest(attemptCtx, env)
		return err
	}

	// First attempt. On transport-level failure (no HTTPError —
	// the daemon never answered: timeout, EOF, connection refused)
	// retry once. Since the 6bd20e5 fix the daemon's enqueue runs
	// on its own decoupled ingestEnqueueBudget=10s, so a hook that
	// gave up at the client deadline (default 5s) commonly times
	// out *while the daemon is still committing the row*. The
	// retry then hits UNIQUE(event_id) on ingest_pending and gets
	// a clean 200 with deduped=true — turning what used to be a
	// phantom "event_dropped" + outage banner into a quiet success.
	//
	// Skipped when:
	//   - The error is an *HTTPError (the daemon answered, including
	//     503 queue-full and any 4xx validation failure). Retrying
	//     wouldn't change the outcome and 503 retries actively make
	//     the backlog worse.
	//   - parentCtx is already canceled (the user hit ^C). Retrying
	//     after the user asked the agent to stop defies intent.
	err = postOnce()
	if err != nil {
		var httpErr *apiclient.HTTPError
		if !errors.As(err, &httpErr) && parentCtx.Err() == nil {
			log.Info("ingest: retrying after transport error",
				"event_id", env.EventID, "first_err", err)
			err = postOnce()
		}
	}
	if err != nil {
		// "event_dropped" is the explicit, greppable key — the
		// previous "post to aichronicles-api" wording buried the
		// fact that the user has just lost an event. Per-line
		// fields (kind, source_session_id) make `journalctl
		// --user-unit=… | grep event_dropped` directly useful for
		// "what did I miss?" forensics.
		log.Error("event_dropped",
			"kind", env.Kind,
			"source_agent", env.SourceAgent,
			"source_session_id", env.SourceSessionID,
			"socket", sockPath,
			"err", err)
		// Daemon-outage signal: any transport-level failure
		// (socket missing, connection refused, timeout, EOF
		// mid-response) flips the outage flag. HTTP-status
		// errors (4xx/5xx with a problem body) usually do NOT —
		// those mean the daemon is up but rejected the envelope,
		// and treating them as "daemon unreachable" would produce
		// false positives on validation drift.
		//
		// Exception: 503 Service Unavailable. The daemon returns
		// 503 when ingest_pending exceeds [limits].ingest_queue_max
		// — i.e. the worker is hopelessly behind. Without this
		// branch, a saturated queue would silently drop every
		// excess event with only a stderr line; the outage flag
		// + desktop banner + recovery count we shipped earlier
		// would never fire. Treat 503 as outage-class so the
		// load condition is visible to the operator even though
		// the daemon technically responded.
		var httpErr *apiclient.HTTPError
		isHTTP := errors.As(err, &httpErr)
		queueFull := isHTTP && httpErr.Status == http.StatusServiceUnavailable
		if !isHTTP || queueFull {
			// Bump the drop counter BEFORE deciding whether to
			// notify — the desktop body line includes the running
			// count so even a debounced banner reflects current
			// damage. Errors here are best-effort: a counter
			// hiccup is strictly less bad than the event we
			// already lost.
			dropCount := 0
			if tracker != nil {
				n, ierr := tracker.Increment()
				if ierr != nil {
					log.Warn("increment outage counter", "err", ierr)
				}
				dropCount = n
			}
			maybeNotifyOutage(log, cfg, tracker, err, dropCount)
		}
		return nil
	}

	// Post succeeded — drop any lingering outage flag so the next
	// failure gets its own notification. If the just-ended outage
	// had recorded any drops, surface a one-shot recovery banner
	// so the user learns about the loss even if they dismissed the
	// outage toast.
	if tracker != nil {
		lostCount, err := tracker.Clear()
		if err != nil {
			log.Warn("clear outage flag", "err", err)
		}
		if lostCount > 0 {
			maybeNotifyRecovery(log, cfg, lostCount)
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
	case "codex-cli":
		tr := &codex.HookTranslator{Now: now}
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

// maybeNotifyOutage sends one desktop notification per outage when
// the user has opted in. The body line embeds dropCount so the
// user sees the loss scale at a glance, even on the renotify
// banner an hour into a long outage. Swallows all errors — no
// notification must ever fail a hook.
func maybeNotifyOutage(log *slog.Logger, cfg *config.Config, tracker *notify.OutageTracker, cause error, dropCount int) {
	if !cfg.Notifications.DaemonUnreachable || tracker == nil || !tracker.ShouldNotify() {
		return
	}
	body := fmt.Sprintf("hooks are losing events (%d dropped so far): %s",
		dropCount, cause.Error())
	if err := notify.New(true).Send("aichronicles: daemon unreachable", body); err != nil {
		log.Warn("notify failed", "err", err)
		return
	}
	if err := tracker.MarkNotified(); err != nil {
		log.Warn("mark outage notified", "err", err)
	}
}

// maybeNotifyRecovery fires a one-shot desktop banner when an
// outage ends with non-zero recorded drops. The first successful
// POST after an outage calls Clear, which returns the accumulated
// count; that's the trigger. Without this banner, a user who
// dismissed the outage toast would have no follow-up signal about
// how much they actually lost.
func maybeNotifyRecovery(log *slog.Logger, cfg *config.Config, lostCount int) {
	if !cfg.Notifications.DaemonUnreachable {
		return
	}
	body := fmt.Sprintf("%d event(s) were lost during the outage", lostCount)
	if err := notify.New(true).Send("aichronicles: daemon recovered", body); err != nil {
		log.Warn("recovery notify failed", "err", err)
	}
}
