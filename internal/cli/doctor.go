package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
)

// defaultDoctorTimeout is how long doctor waits for the daemon to
// respond before declaring it unreachable. Tighter than the ingest
// timeout because doctor is interactive — the user is watching for
// a fast verdict, not racing a hook deadline.
const defaultDoctorTimeout = 1 * time.Second

// doctorWriter abstracts cmd.OutOrStdout / cmd.ErrOrStderr so
// RunDoctor stays trivially testable.
type doctorWriter interface {
	io.Writer
}

func newDoctorCmd() *cobra.Command {
	var socketFlag string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Probe the running daemon and report whether it is accepting events",
		Long: "doctor performs a real connect + roundtrip against the\n" +
			"daemon's UDS healthz endpoint and reports whether ingest\n" +
			"would currently succeed. Exits 0 when the daemon answers,\n" +
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
				// Errors from RunDoctor are already user-facing; cobra's
				// SilenceErrors=true means we surface them ourselves.
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aichronicles doctor:", err)
			}
			if !ok {
				// Non-zero exit so callers (status bars, cron) can
				// branch on health without parsing stdout.
				return errExitCodeOne
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socketFlag, "socket", "", "daemon UDS path (overrides $AICHRONICLES_SOCKET; defaults to XDG_RUNTIME_DIR)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress all output; use the exit code only")
	return cmd
}

// errExitCodeOne is a sentinel cobra renders as a non-zero exit
// without printing anything (we already wrote our own diagnostic).
var errExitCodeOne = errors.New("daemon unhealthy")

// RunDoctor probes the daemon at sockPath (or the resolved default
// when sockPath is empty) and writes a one-line verdict to w. The
// boolean return reports daemon health: true when the healthz
// endpoint returned 2xx within defaultDoctorTimeout, false on any
// connection or response error.
//
// Exported so integration tests and the e2e harness can drive it
// without forking a binary.
func RunDoctor(ctx context.Context, w doctorWriter, socketFlag string, quiet bool) (bool, error) {
	sockPath, err := paths.ResolveSocketPath(socketFlag)
	if err != nil {
		return false, fmt.Errorf("resolve socket path: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, defaultDoctorTimeout)
	defer cancel()

	client := NewClient(sockPath)
	req, err := http.NewRequestWithContext(dialCtx,
		http.MethodGet, "http://unix/v1/healthz", nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.HTTP.Do(req)
	if err != nil {
		if !quiet {
			_, _ = fmt.Fprintf(w, "FAIL daemon unreachable at %s: %v\n", sockPath, err)
		}
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode/100 != 2 {
		if !quiet {
			_, _ = fmt.Fprintf(w, "FAIL daemon at %s returned %d: %s\n",
				sockPath, resp.StatusCode, string(body))
		}
		return false, nil
	}

	if !quiet {
		_, _ = fmt.Fprintf(w, "OK daemon at %s healthy\n", sockPath)
	}
	return true, nil
}
