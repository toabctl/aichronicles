package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	sdDaemon "github.com/coreos/go-systemd/v22/daemon"
)

// defaultProbeTimeout caps the in-process UDS roundtrip the
// watchdog uses to prove the accept loop is alive. healthz is a
// literal {"status":"ok"} write — it has no DB call, no I/O, no
// allocation of consequence — so 500ms is comically generous.
// Picked that loose so a GC pause spike can't trigger a spurious
// restart.
const defaultProbeTimeout = 500 * time.Millisecond

// Watchdog drives systemd's WATCHDOG protocol. The headline design
// decision: each ping is gated on a real GET /v1/healthz roundtrip
// against the daemon's own UDS, so the daemon can't notify "I'm
// alive" while its accept loop is wedged. That's the exact failure
// mode that cost 30 hours of dropped events; a heartbeat goroutine
// that pinged unconditionally would have papered over it.
//
// Concurrency: not safe for concurrent use; one Watchdog per
// daemon instance, owned by Start. Run blocks until ctx cancels.
type Watchdog struct {
	// Interval is how often the watchdog probes + pings. Set to
	// half the systemd-supplied WATCHDOG_USEC by Start so a
	// single missed probe still leaves headroom before systemd's
	// deadline trips.
	Interval time.Duration

	// ProbeTimeout caps how long one in-process /v1/healthz
	// roundtrip is allowed to take before we treat it as
	// unhealthy and skip the ping. Independent of Interval so
	// the probe doesn't bleed into the next tick.
	ProbeTimeout time.Duration

	// SockPath is the daemon's own listening UDS. The probe
	// dials this path, mirroring exactly what a hook does.
	SockPath string

	// Notify is the systemd sd_notify callback. Injectable so
	// tests can observe pings without poking $NOTIFY_SOCKET. In
	// production it's sdDaemon.SdNotify.
	Notify func(state string) (sent bool, err error)

	// Log is the structured logger. Watchdog warnings (probe
	// failures, panics) emit here at slog.LevelWarn so a
	// permanently-failing probe is at least audit-trail-visible
	// even when the desktop notifier silently drops banners.
	Log *slog.Logger
}

// Start inspects WATCHDOG_USEC and either starts the watchdog
// goroutine bound to ctx, or returns silently when systemd's
// watchdog isn't enabled (manual run, tests). The goroutine
// exits when ctx cancels, when the Notify callback errors
// fatally, or after a recovered panic.
//
// Returns nil in all "no watchdog" cases — the caller should
// treat the absence of a watchdog as configured-off, not an
// error.
func Start(ctx context.Context, sockPath string, log *slog.Logger) error {
	interval, err := sdDaemon.SdWatchdogEnabled(false)
	if err != nil {
		// Misconfigured WATCHDOG_PID — surface as a warning,
		// don't crash. The daemon is more useful running
		// without watchdog supervision than not running at all.
		log.Warn("systemd watchdog setup", "err", err)
		return nil
	}
	if interval == 0 {
		// systemd didn't enable watchdog (no WATCHDOG_USEC, or
		// not socket-activated). Nothing to do.
		return nil
	}

	w := &Watchdog{
		Interval:     interval / 2,
		ProbeTimeout: defaultProbeTimeout,
		SockPath:     sockPath,
		Notify: func(state string) (bool, error) {
			return sdDaemon.SdNotify(false, state)
		},
		Log: log,
	}
	go w.Run(ctx)
	log.Info("systemd watchdog enabled", "interval", w.Interval, "deadline", interval)
	return nil
}

// Run blocks until ctx cancels. Each tick: probe own UDS at
// /v1/healthz; on 2xx send WATCHDOG=1; on any failure (timeout,
// non-2xx, dial error) stay silent and let systemd's deadline
// trip.
//
// A panic inside the loop is recovered + logged so a bug in the
// ping path doesn't silently strand the goroutine — that would
// produce exactly the "process up, no pings" state we'd misread
// as the daemon's-stuck condition we built this to catch.
func (w *Watchdog) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.Log.Error("watchdog goroutine panicked", "panic", r)
		}
	}()

	tick := time.NewTicker(w.Interval)
	defer tick.Stop()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", w.SockPath)
			},
		},
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if w.probeOnce(ctx, client) {
				if _, err := w.Notify("WATCHDOG=1"); err != nil {
					w.Log.Error("watchdog notify failed", "err", err)
					return
				}
			}
		}
	}
}

// probeOnce runs one in-process UDS roundtrip. Returns true iff
// the server answered 2xx within w.ProbeTimeout. Logs failures at
// warn level so a flapping daemon shows up in the journal even
// when the pings successfully resume on the next tick.
func (w *Watchdog) probeOnce(ctx context.Context, client *http.Client) bool {
	probeCtx, cancel := context.WithTimeout(ctx, w.ProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet,
		"http://unix/v1/healthz", nil)
	if err != nil {
		w.Log.Warn("watchdog: build request", "err", err)
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		w.Log.Warn("watchdog: probe failed", "err", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		w.Log.Warn("watchdog: probe returned non-2xx", "status", resp.StatusCode)
		return false
	}
	return true
}

// notifyReady sends READY=1 to systemd. No-op when not under a
// notify-type service.
func notifyReady(log *slog.Logger) {
	if _, err := sdDaemon.SdNotify(false, "READY=1"); err != nil {
		log.Warn("sd_notify READY=1", "err", err)
	}
}

// notifyStopping sends STOPPING=1 to systemd. Lets systemd
// distinguish a clean shutdown from a watchdog-driven kill in
// the journal.
func notifyStopping(log *slog.Logger) {
	if _, err := sdDaemon.SdNotify(false, "STOPPING=1"); err != nil {
		log.Warn("sd_notify STOPPING=1", "err", err)
	}
}

// NotifyReady / NotifyStopping are public wrappers so
// cmd/aichronicles-api can call into them without re-importing
// sdDaemon.
func NotifyReady(log *slog.Logger)    { notifyReady(log) }
func NotifyStopping(log *slog.Logger) { notifyStopping(log) }
