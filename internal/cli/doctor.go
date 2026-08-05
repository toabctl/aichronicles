package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultDoctorTimeout is how long doctor waits for the daemon to
// respond before declaring it unreachable. Tighter than the
// ingest timeout because doctor is interactive — the user is
// watching for a fast verdict, not racing a hook deadline.
const defaultDoctorTimeout = 1 * time.Second

// pipelineStaleAfter is how old the newest LLM artifact may be before
// doctor calls the pipeline stale.
//
// The induction timer fires twice a day and meta-analysis hourly, so a
// working install produces something within hours. Three days is well
// past that while still tolerating a long weekend away from the
// machine, a suspended laptop, or a deliberate pause.
//
// This check exists because the pipeline has now died silently twice.
// Both times capture kept working — events flowed, `doctor` reported a
// healthy daemon, the web UI listed new sessions — while nothing was
// being summarised. The second outage ran 34 days and was noticed only
// by accident. A healthy daemon is not a healthy system: the daemon is
// the data plane, and every failure so far has been in the control
// plane the daemon knows nothing about.
const pipelineStaleAfter = 72 * time.Hour

func newDoctorCmd() *cobra.Command {
	var socketFlag string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Probe the running aichronicles-api daemon and report health",
		Long: "doctor performs a real connect + roundtrip against the\n" +
			"daemon's UDS healthz endpoint and reports whether the api\n" +
			"is currently answering. Exits 0 when the daemon answers,\n" +
			"non-zero otherwise, so the command can be wired to a\n" +
			"status bar, a cron job, or a shell prompt indicator.\n\n" +
			"Catches the failure mode where the daemon process is\n" +
			"running and the kernel reports the socket as LISTEN but\n" +
			"connect() actually returns ECONNREFUSED — which silently\n" +
			"drops every hook fire and is invisible to `pgrep` or\n" +
			"`systemctl status`.\n\n" +
			"Also reports on the ANALYSIS pipeline, which is a separate\n" +
			"failure domain from the daemon: whether the cron timers are\n" +
			"installed, and how old the newest LLM artifact is. A healthy\n" +
			"daemon keeps capturing events whether or not anything is\n" +
			"summarising them, so those two can be broken for weeks\n" +
			"without any other signal. These surface as WARN lines and do\n" +
			"NOT change the exit code — the daemon probe alone decides\n" +
			"that, so an existing status-bar or prompt integration keeps\n" +
			"its meaning.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ok, err := RunDoctor(cmd.Context(), cmd.OutOrStdout(), socketFlag, quiet)
			if err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aichronicles doctor:", err)
			}
			if !ok {
				return errExitCodeOne
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"suppress all output; use the exit code only")
	return cmd
}

// errExitCodeOne is a sentinel cobra renders as a non-zero exit
// without printing anything (we already wrote our own diagnostic).
var errExitCodeOne = errors.New("daemon unhealthy")

// RunDoctor probes aichronicles-api at the resolved socket and
// writes a one-line verdict to w. The boolean return reports
// daemon health: true when /v1/healthz returned 2xx within
// defaultDoctorTimeout, false on any connection or response error.
//
// Exported so integration tests and the e2e harness can drive it
// without forking a binary.
func RunDoctor(ctx context.Context, w io.Writer, socketFlag string, quiet bool) (bool, error) {
	sockPath, err := paths.ResolveAPISocketPath(socketFlag)
	if err != nil {
		return false, fmt.Errorf("resolve api socket path: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, defaultDoctorTimeout)
	defer cancel()

	c := apiclient.NewClient(sockPath)
	if err := c.Healthz(dialCtx); err != nil {
		if !quiet {
			_, _ = fmt.Fprintf(w, "FAIL daemon unreachable at %s: %v\n", sockPath, err)
		}
		return false, nil
	}
	if !quiet {
		_, _ = fmt.Fprintf(w, "OK daemon at %s healthy\n", sockPath)
		reportPipelineHealth(ctx, w, c)
	}
	return true, nil
}

// reportPipelineHealth writes WARN lines for the analysis pipeline.
//
// Deliberately advisory: it never changes doctor's exit code and never
// returns an error. The daemon probe owns the verdict (that is doctor's
// documented contract, and things depend on it), while these checks
// answer a different question — "is anything actually thinking about
// the data I'm capturing?"
//
// Both checks are cheap and neither is fatal if it can't run: a probe
// that fails to probe should stay quiet rather than cry wolf.
func reportPipelineHealth(ctx context.Context, w io.Writer, c *apiclient.Client) {
	reportCronTimers(w)
	reportPipelineStaleness(ctx, w, c)
}

// reportCronTimers warns when no cron unit files are installed.
//
// This is the check that would have caught the real outage. Recovery
// after a machine reinstall needs TWO commands — `setup systemd` for
// the daemon and `setup cron` for the timers — and only the first has
// visible consequences. Run just that one and capture resumes, the UI
// works, doctor says healthy, and nothing is ever analysed again.
//
// Looks at the filesystem rather than asking systemd, so it works
// without a session bus and reports the durable state (installed)
// rather than the transient one (currently running).
func reportCronTimers(w io.Writer) {
	// Reuse the resolver setup/teardown use, so doctor can never
	// look in a different directory than the installer writes to.
	dir, err := defaultSystemdUserDir()
	if err != nil {
		return
	}
	matches, err := filepath.Glob(filepath.Join(dir, "aichronicles-cron-*.timer"))
	if err != nil {
		return
	}
	if len(matches) == 0 {
		_, _ = fmt.Fprintf(w,
			"WARN no cron timers installed — nothing will summarise, reflect or "+
				"propose. Run `aichronicles setup cron`.\n")
	}
}

// reportPipelineStaleness warns when the newest LLM artifact is older
// than pipelineStaleAfter.
//
// Silent on an empty store: no artifacts plus no sessions is a fresh
// install, not a stall. Warning there would train the reader to ignore
// the line, which is how a real warning gets missed later.
func reportPipelineStaleness(ctx context.Context, w io.Writer, c *apiclient.Client) {
	outs, err := c.LLMOutputsList(ctx, "", "", 1)
	if err != nil {
		return
	}
	if len(outs) == 0 {
		// Distinguish "nothing captured yet" from "captured plenty,
		// analysed none" — only the second is a problem.
		resp, sErr := c.Sessions(ctx, wire.SessionListRequest{Limit: 1, SinceMs: 1})
		if sErr == nil && len(resp.Sessions) > 0 {
			_, _ = fmt.Fprintf(w,
				"WARN %d+ session(s) captured but no LLM artifacts exist — the "+
					"analysis pipeline has never run.\n", len(resp.Sessions))
		}
		return
	}
	age := time.Since(time.UnixMilli(outs[0].CreatedAtMs))
	if age > pipelineStaleAfter {
		_, _ = fmt.Fprintf(w,
			"WARN newest LLM artifact is %d days old (%s) — the analysis pipeline "+
				"looks stalled. Check `journalctl --user -u "+
				"aichronicles-cron-meta-analysis.service` and that the provider "+
				"API key still resolves.\n",
			int(age.Hours()/24), time.UnixMilli(outs[0].CreatedAtMs).UTC().Format("2006-01-02"))
	}
}
