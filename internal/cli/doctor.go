package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/paths"
)

// defaultDoctorTimeout is how long doctor waits for the daemon to
// respond before declaring it unreachable. Tighter than the
// ingest timeout because doctor is interactive — the user is
// watching for a fast verdict, not racing a hook deadline.
const defaultDoctorTimeout = 1 * time.Second

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
			"`systemctl status`.",
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
	}
	return true, nil
}
