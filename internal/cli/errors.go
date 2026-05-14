package cli

import (
	"errors"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
)

// ErrEmptyWindow is the sentinel propose / reflect / digest commands
// wrap when the requested window has too few summarised sessions to
// build a prompt. The meta-analysis sweeper treats it as a quiet-
// system signal rather than a failure so an empty week doesn't fill
// the operator's log with noise.
var ErrEmptyWindow = errors.New("empty window")

// hintForError returns a one-line follow-up when err matches a known
// failure mode the user can act on. Empty string when no hint applies.
//
// Hints are deliberately stable, short, and concrete: they tell the
// user what to type next, not why the error happened. Avoid stacking
// — only the first matching rule fires so the user reads one
// suggestion, not three.
func hintForError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, store.ErrNoSuchSession) {
		return "hint: run `aichronicles sessions` to list sessions; the first column is the prefix this command accepts."
	}
	if errors.Is(err, store.ErrAmbiguousSessionPrefix) {
		return "hint: pass a longer prefix to uniquely identify the session."
	}
	if errors.Is(err, llm.ErrNoAPIKey) {
		return "hint: export ANTHROPIC_API_KEY / OPENAI_API_KEY, or set [llm.<provider>].api_key_command in `~/.config/aichronicles/config.toml` (chmod 600)."
	}
	if errors.Is(err, apiclient.ErrSocketUnavailable) {
		return "hint: check `systemctl --user status aichronicles-api.service` and `aichronicles setup systemd` to (re)install the units."
	}
	return ""
}
