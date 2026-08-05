// Package resumecmd renders the invocation that resumes a captured
// agent session in its original workspace. It centralises the
// agent-specific knowledge — which binary to launch, which resume flag
// it takes, whether a permission-skip flag exists — so the web "Resume"
// buttons and the CLI `resume` command stay in lockstep instead of each
// hand-rolling the same `claude --resume <id>` string.
//
// The package never guesses: an agent it can't model, a session with no
// upstream id, or a permission-skip request for an agent without such a
// flag all yield ok=false so the caller hides the affordance rather than
// emit a command the binary will reject.
package resumecmd

import "strings"

// Spec is a resolved resume invocation. Bin + Args are the structured
// form the CLI execs directly; Cwd is the directory the agent must start
// in. The cwd matters: `claude --resume` indexes transcripts by the
// session's *start* cwd, not the latest one, so callers pass the start
// cwd here (see store/events.go for why latest-cwd resume fails).
type Spec struct {
	// Cwd is the workspace to enter before launching. Empty means
	// "launch in the current directory" (no cd).
	Cwd string
	// Bin is the agent binary to exec ("claude", "gemini").
	Bin string
	// Args are the arguments after Bin, e.g. ["--resume", "<uuid>"].
	Args []string
}

// Build resolves the resume invocation for (agent, sourceSessionID,
// cwd). ok is false — and Spec is zero — when we can't model a correct
// invocation: an empty source session id, an agent we don't recognise,
// or skipPerms requested for an agent that has no permission-skip flag.
// Callers branch on ok to hide the resume affordance.
func Build(agent, sourceSessionID string, cwd *string, skipPerms bool) (Spec, bool) {
	if sourceSessionID == "" {
		return Spec{}, false
	}

	args := []string{"--resume", sourceSessionID}
	var bin string
	switch agent {
	case "claude-code":
		bin = "claude"
		if skipPerms {
			args = append(args, "--dangerously-skip-permissions")
		}
	case "gemini-cli":
		// gemini-cli's `--help` advertises `--resume <index>` /
		// `--resume latest`, but the binary also accepts the session
		// UUID directly (verified end-to-end), which is stable across
		// new sessions unlike the index. Its permission bypass is a
		// different flag we don't model, so a skipPerms request can't
		// be honoured.
		if skipPerms {
			return Spec{}, false
		}
		bin = "gemini"
	default:
		// codex / other agents have their own resume invocations we
		// haven't modelled yet; emit nothing rather than guess.
		return Spec{}, false
	}

	var dir string
	if cwd != nil {
		dir = *cwd
	}
	return Spec{Cwd: dir, Bin: bin, Args: args}, true
}

// Shell renders the spec as a copy-pasteable one-liner: `cd <cwd> &&
// <bin> <args...>`, collapsing to `<bin> <args...>` when Cwd is empty.
// This is the exact form the web Resume button copies to the clipboard
// and the CLI `resume --print` flag emits.
//
// Every interpolated value is single-quoted. This is the one place in
// the resume path where the trust boundary changes: the exec path
// hands Bin and Args to syscall.Exec as a structured argv, where
// quoting would be wrong, while the same fields here are pasted into
// a shell, where its absence is.
//
// Unquoted, the everyday failure needs no attacker — a workspace at
// "/home/u/My Projects/api" renders `cd /home/u/My Projects/api && …`,
// which cds into "/home/u/My" and then runs "Projects/api" as a
// command. cwd comes from the hook payload and is never validated,
// and Envelope.Validate only checks that source_session_id is
// non-empty, so a directory name containing ';' or '$(…)' turns a
// clipboard paste into arbitrary execution.
func (s Spec) Shell() string {
	parts := make([]string, 0, len(s.Args)+1)
	parts = append(parts, shellQuote(s.Bin))
	for _, a := range s.Args {
		parts = append(parts, shellQuote(a))
	}
	base := strings.Join(parts, " ")
	if s.Cwd != "" {
		return "cd " + shellQuote(s.Cwd) + " && " + base
	}
	return base
}

// shellQuote wraps s in single quotes. An embedded single quote is
// closed, escaped and reopened (the classic quote-backslash-quote-
// quote sequence). Inside single quotes POSIX shells treat every
// other byte literally, so this is safe for arbitrary content.
//
// Values that are already unambiguous — the common case, a plain path
// or a UUID — are returned bare so the copied line stays readable.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafe reports whether s can appear unquoted without changing
// meaning in any POSIX shell. Allowlist, not denylist: the set of
// characters a shell treats specially is long, version-dependent and
// easy to under-enumerate, whereas the set that is always literal is
// short and stable.
func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '_' || r == '-' || r == '=' || r == ':' || r == '+' || r == ',':
		default:
			return false
		}
	}
	return true
}
