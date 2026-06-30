package cli

import (
	"io"
	"os"
)

// EnvNoColor follows the de-facto standard at https://no-color.org —
// any non-empty value disables ANSI codes regardless of TTY state.
const EnvNoColor = "NO_COLOR"

// isCharDevice reports whether w is the terminal (a character device)
// rather than a pipe, redirected file, or in-memory buffer. Driven off
// os.File.Stat so we never grow a dependency on golang.org/x/term for
// what is a one-line check.
func isCharDevice(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return fileIsCharDevice(f)
}

// fileIsCharDevice reports whether f is a terminal (character device)
// rather than a pipe or redirected file. Used to decide whether stdin
// can be prompted interactively (the `resume` picker) — a check that
// needs the *os.File directly, not the io.Writer isCharDevice takes.
func fileIsCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// useColor reports whether ANSI escape codes are appropriate when
// rendering to w. False when the user set NO_COLOR (any value), false
// when w is not a TTY (pipes, redirects, in-memory buffers in tests).
// The combination is what the major CLIs (gh, kubectl, git) settled
// on.
func useColor(w io.Writer) bool {
	if os.Getenv(EnvNoColor) != "" {
		return false
	}
	return isCharDevice(w)
}

// ANSI sequences. Kept minimal: bold for section headers, dim for
// trailing context, red for the error prefix. We deliberately do not
// expose a palette — every additional colour is a future regression
// against a colourblind user or a mis-configured terminal.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// styled wraps s in ANSI codes when w warrants colour, otherwise
// returns s unchanged. Treat as the only place in the CLI that emits
// ANSI bytes — every other rendering path should call through here so
// "no colour" is one switch, not many.
func styled(w io.Writer, s, ansi string) string {
	if !useColor(w) {
		return s
	}
	return ansi + s + ansiReset
}
