// Package textfmt holds the small, dependency-free string helpers
// shared across packages that render human- or markdown-facing text
// (the CLI's propose index, the skillscaffold SKILL.md renderer).
// They used to live as unexported helpers in internal/cli; once the
// SKILL.md rendering moved to internal/skillscaffold (which cli and
// web both import) the helpers had to move somewhere both could
// reach without an import cycle.
package textfmt

import "strings"

// CollapseWhitespace squashes runs of whitespace (newlines,
// multiple spaces) into single spaces so a multi-line value fits on
// one line in a report or comment. Leading/trailing whitespace is
// collapsed to a single space, not trimmed — callers that want it
// gone should TrimSpace first (see OneLine).
func CollapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

// OneLine trims then collapses any whitespace run (newlines
// included) into single spaces so a multi-line field renders
// cleanly inside a list item or comment.
func OneLine(s string) string {
	return CollapseWhitespace(strings.TrimSpace(s))
}

// OneLineN is OneLine plus a rune-count cap. Used for YAML scalars
// (kept short to stay readable) and for list previews.
func OneLineN(s string, n int) string {
	out := OneLine(s)
	r := []rune(out)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return out
}

// ClipToRunes is a rune-safe truncation that lands cleanly on a
// word boundary when possible. Used for frontmatter scalars where
// Claude Code caps combined description + when_to_use at 1536 chars.
func ClipToRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndexAny(cut, " ,;:"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

// LowerFirst lowercases the first rune of s, leaving the rest
// untouched. Used to splice script purposes into "Run scripts/foo
// to <purpose>" so the resulting sentence reads naturally.
func LowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToLower(string(r[0])))[0]
	return string(r)
}
