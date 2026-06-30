package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/resumecmd"
	"github.com/toabctl/aichronicles/internal/wire"
)

// defaultResumeWindow bounds the search to recently-active sessions by
// default. Resume is for picking up where you left off, so the answer
// you want is almost always recent; capping at 6 weeks keeps the picker
// short and the FTS query cheap. Pass --since 0 to search all history.
const defaultResumeWindow = 6 * 7 * 24 * time.Hour

func newResumeCmd() *cobra.Command {
	var (
		limit     int
		since     time.Duration
		agent     string
		printOnly bool
		skipPerms bool
		sockFlag  string
	)
	cmd := &cobra.Command{
		Use:   "resume <query>",
		Short: "Search sessions and resume the chosen one in its workspace",
		Long: "Full-text searches captured sessions for <query>, lists the\n" +
			"best matches one per line (when, short id, cwd, opening\n" +
			"prompt), and prompts you to pick one. The chosen session is\n" +
			"resumed by launching its agent (`claude --resume <id>` /\n" +
			"`gemini --resume <id>`) after cd-ing into the workspace the\n" +
			"session started in — `claude --resume` indexes transcripts by\n" +
			"start cwd, so this is what makes resume actually find the\n" +
			"conversation.\n\n" +
			"The current process is replaced by the agent, so you land\n" +
			"directly in the resumed session. Pass --print to emit the\n" +
			"resume one-liners instead of launching (also the automatic\n" +
			"behavior when stdin is not a terminal, so it composes with\n" +
			"pipes). Sessions whose agent we can't model are omitted —\n" +
			"resume only lists what it can actually relaunch.\n\n" +
			"By default only sessions active in the last 6 weeks are\n" +
			"considered; widen or disable the window with --since (e.g.\n" +
			"--since 90d, or --since 0 for no limit).\n\n" +
			"Talks to aichronicles-api over its UDS (override with\n" +
			"--socket or $AICHRONICLES_API_SOCKET).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			opts := ResumeOptions{
				Query:       args[0],
				Limit:       limit,
				Agent:       agent,
				Print:       printOnly,
				SkipPerms:   skipPerms,
				Interactive: fileIsCharDevice(os.Stdin),
			}
			if since > 0 {
				opts.SinceMs = time.Now().Add(-since).UnixMilli()
			}
			return RunResume(cmd.Context(), c, opts, cmd.InOrStdin(), cmd.OutOrStdout(), execResume)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 10, "max matching sessions to list")
	addFlexDurationFlag(cmd, &since, "since", defaultResumeWindow, "only sessions with events within this window (e.g. 24h, 7d); 0 = no limit")
	cmd.Flags().StringVar(&agent, "agent", "", "filter by source agent (claude-code | gemini-cli)")
	cmd.Flags().BoolVarP(&printOnly, "print", "n", false, "print the resume command(s) instead of launching the agent")
	cmd.Flags().BoolVarP(&skipPerms, "skip-permissions", "d", false, "(dangerous) resume with --dangerously-skip-permissions (claude-code only)")
	addSocketFlag(cmd, &sockFlag)
	return cmd
}

// ResumeOptions are the flag values passed to RunResume. Exported so
// tests can drive the same path without going through cobra.
type ResumeOptions struct {
	Query     string
	Limit     int
	SinceMs   int64
	Agent     string
	Print     bool // print the resume command instead of launching
	SkipPerms bool // append --dangerously-skip-permissions (claude-code)
	// Interactive reports whether stdin is a terminal we can prompt on.
	// When false, RunResume falls back to printing the commands rather
	// than blocking on a read that will never get an answer.
	Interactive bool
}

// resumeExecFn launches (and replaces this process with) the resolved
// agent invocation. Injected so tests can capture the chosen spec
// instead of actually exec-ing claude.
type resumeExecFn func(resumecmd.Spec) error

// resumeCandidate pairs a matched session's digest (for display) with
// its resolved resume invocation (for launch / print).
type resumeCandidate struct {
	digest wire.SessionDigest
	spec   resumecmd.Spec
}

// RunResume searches for sessions matching opts.Query, lists the
// resumable ones, and either launches the chosen agent (interactive) or
// prints the resume command(s) (--print / non-TTY). The exec is
// injected so the interactive path is testable up to the launch
// boundary.
func RunResume(
	ctx context.Context,
	c *apiclient.Client,
	opts ResumeOptions,
	in io.Reader,
	out io.Writer,
	execFn resumeExecFn,
) error {
	if strings.TrimSpace(opts.Query) == "" {
		return errors.New("resume query must not be empty")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}

	// Pull more FTS hits than the session cap: one session can own many
	// matching turns, so we over-fetch then dedup to distinct sessions.
	searchLimit := limit * 8
	if searchLimit < 40 {
		searchLimit = 40
	}
	resp, err := c.Search(ctx, wire.SearchRequest{
		Q:           opts.Query,
		SourceAgent: opts.Agent,
		SinceMs:     opts.SinceMs,
		Limit:       searchLimit,
	})
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	// Distinct session ids in relevance order (best-matching hit first).
	order := make([]string, 0, len(resp.Hits))
	seen := make(map[string]bool, len(resp.Hits))
	for _, h := range resp.Hits {
		if h.SessionID == "" || seen[h.SessionID] {
			continue
		}
		seen[h.SessionID] = true
		order = append(order, h.SessionID)
	}
	if len(order) == 0 {
		_, err := fmt.Fprintf(out, "(no sessions matched %q)\n", opts.Query)
		return err
	}

	digests, err := c.SessionDigestsByIDs(ctx, order)
	if err != nil {
		return fmt.Errorf("load session digests: %w", err)
	}

	cands := make([]resumeCandidate, 0, limit)
	skippedAny := false
	for _, id := range order {
		if len(cands) >= limit {
			break
		}
		d, ok := digests[id]
		if !ok {
			continue
		}
		// `claude --resume` keys off the session's *start* cwd, not the
		// latest one; fall back to latest only when start wasn't captured.
		resumeCwd := d.StartCwd
		if resumeCwd == nil {
			resumeCwd = d.Cwd
		}
		spec, ok := resumecmd.Build(d.SourceAgent, d.SourceSessionID, resumeCwd, opts.SkipPerms)
		if !ok {
			skippedAny = true
			continue
		}
		cands = append(cands, resumeCandidate{digest: d, spec: spec})
	}

	if len(cands) == 0 {
		_, err := fmt.Fprintf(out,
			"(matched sessions for %q, but none can be resumed%s)\n",
			opts.Query, skipPermsNote(opts.SkipPerms))
		return err
	}

	if err := renderResumeTable(out, cands); err != nil {
		return err
	}
	if skippedAny {
		_, _ = fmt.Fprintln(out, "(some matching sessions can't be resumed — unknown agent or missing session id — and were skipped)")
	}

	// Launch only when we have a terminal to prompt on and --print
	// wasn't requested; otherwise show the commands and stop.
	if opts.Print || !opts.Interactive {
		if !opts.Interactive && !opts.Print {
			_, _ = fmt.Fprintln(out, "(stdin is not a terminal; printing resume commands — run in a terminal to pick one)")
		}
		return printResumeCommands(out, cands)
	}

	choice, ok, err := promptResumeChoice(in, out, len(cands))
	if err != nil {
		return err
	}
	if !ok {
		_, err := fmt.Fprintln(out, "(cancelled)")
		return err
	}
	chosen := cands[choice-1]
	_, _ = fmt.Fprintf(out, "→ %s\n", chosen.spec.Shell())
	return execFn(chosen.spec)
}

// renderResumeTable writes the numbered candidate list through a
// tabwriter so columns line up: #  WHEN  SESSION  CWD  FIRST_PROMPT.
func renderResumeTable(out io.Writer, cands []resumeCandidate) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "#\tWHEN\tSESSION\tCWD\tFIRST_PROMPT"); err != nil {
		return err
	}
	now := time.Now()
	for i, c := range cands {
		d := c.digest
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			i+1,
			resumeWhen(d, now),
			preview.ShortID(d.ID),
			strPtrOrDash(d.Cwd),
			truncatePrompt(strPtrOrDash(d.FirstPrompt)),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.Copy(out, &buf)
	return err
}

// printResumeCommands emits one copy-pasteable resume one-liner per
// candidate, numbered to match the table above it.
func printResumeCommands(out io.Writer, cands []resumeCandidate) error {
	for i, c := range cands {
		if _, err := fmt.Fprintf(out, "  [%d] %s\n", i+1, c.spec.Shell()); err != nil {
			return err
		}
	}
	return nil
}

// promptResumeChoice prompts until it reads a valid 1..n selection, a
// cancel (blank line or "q"), or EOF. The returned bool is false on
// cancel/EOF; the int is the 1-based choice when true.
func promptResumeChoice(in io.Reader, out io.Writer, n int) (int, bool, error) {
	r := bufio.NewReader(in)
	for {
		_, _ = fmt.Fprintf(out, "Resume which session? [1-%d], q to cancel: ", n)
		line, readErr := r.ReadString('\n')
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.EqualFold(trimmed, "q") {
			return 0, false, nil
		}
		if choice, err := strconv.Atoi(trimmed); err == nil && choice >= 1 && choice <= n {
			return choice, true, nil
		}
		_, _ = fmt.Fprintf(out, "  not a valid choice: %q\n", trimmed)
		if readErr != nil {
			// Bad token followed by EOF (closed stdin) — stop rather
			// than spin forever re-prompting against an empty reader.
			return 0, false, nil
		}
	}
}

// resumeWhen renders the session's effective timestamp (ended_at when
// set, else started_at) in the CLI's relative form. "-" when neither
// is recorded (a mid-flight session that never closed a turn).
func resumeWhen(d wire.SessionDigest, now time.Time) string {
	ts := d.EndedAtMs
	if ts == nil {
		ts = d.StartedAtMs
	}
	if ts == nil {
		return "-"
	}
	return formatTimeForUser(*ts, now)
}

// skipPermsNote annotates the "none can be resumed" message when
// --skip-permissions narrowed the field (gemini-cli / unknown agents
// have no such flag, so they're excluded under it).
func skipPermsNote(skip bool) string {
	if skip {
		return " with --skip-permissions (only claude-code supports it)"
	}
	return ""
}

// execResume replaces the current process with the resolved agent
// invocation, after cd-ing into the session's workspace. syscall.Exec
// only returns on failure (a successful exec never comes back), so any
// non-nil return here is a real launch error.
func execResume(spec resumecmd.Spec) error {
	bin, err := exec.LookPath(spec.Bin)
	if err != nil {
		return fmt.Errorf("cannot find %q on PATH: %w", spec.Bin, err)
	}
	if spec.Cwd != "" {
		if err := os.Chdir(spec.Cwd); err != nil {
			return fmt.Errorf("cd %s: %w", spec.Cwd, err)
		}
	}
	argv := append([]string{spec.Bin}, spec.Args...)
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", spec.Bin, err)
	}
	return nil
}
