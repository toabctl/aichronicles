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
func (s Spec) Shell() string {
	base := s.Bin + " " + strings.Join(s.Args, " ")
	if s.Cwd != "" {
		return "cd " + s.Cwd + " && " + base
	}
	return base
}
