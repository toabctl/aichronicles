// Package searchquery converts user-facing search input into a
// syntactically-safe FTS5 MATCH expression.
//
// Callers (the `aichronicles search` CLI and the MCP `search_events`
// tool) accept arbitrary text from a human or an agent. FTS5's MATCH
// syntax is finicky — bare punctuation, stray quotes, or a leading
// `-` can either error out or quietly mean something different. This
// package is the single chokepoint that translates plain words into
// a query the database is happy to run.
//
// Behaviour summary:
//
//   - Bare tokens get a trailing `*` so partial matches grow naturally
//     ("shutdown" finds "shutdowns").
//   - "Quoted phrases" are preserved as FTS5 phrases.
//   - Tokens containing FTS5 specials or the tokenizer's separator
//     characters (`_`, `-`, `.`, `/`) are wrapped as a phrase so the
//     query parser doesn't choke and the tokenizer splits them on its
//     own terms.
//   - Multiple parts are joined with implicit AND (FTS5's default).
//
// User-typed FTS5 operators (AND, OR, NOT, parens) are NOT honoured;
// they are treated as content tokens. This is intentional — agents
// and humans expect Google-style search, not Lucene.
package searchquery

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrEmpty is returned when input is empty or whitespace-only.
var ErrEmpty = errors.New("search query is empty")

// ErrSyntax is returned for malformed input we can't safely transform,
// e.g. an unclosed double quote.
var ErrSyntax = errors.New("search query has invalid syntax")

// separators are the characters the SQLite FTS5 tokenizer treats as
// token separators in our schema (see migration 004). Tokens that
// contain any of them must be wrapped in quotes to survive parsing
// and to let the tokenizer split them itself.
const separators = "_-./"

// fts5Specials are characters with special meaning in an FTS5 MATCH
// expression. Tokens containing any of these are wrapped as a phrase.
const fts5Specials = `"():*^+`

// ToFTS5 transforms a user-facing query string into an FTS5 MATCH
// expression. See the package doc comment for the rules.
func ToFTS5(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ErrEmpty
	}
	parts, err := tokenize(input)
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t, err := transform(p)
		if err != nil {
			return "", err
		}
		if t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return "", ErrEmpty
	}
	return strings.Join(out, " "), nil
}

// part is one tokenized piece of input — either a bare run of
// non-whitespace characters, or a quoted phrase (with the quotes
// stripped from its body).
type part struct {
	body   string
	quoted bool
}

// tokenize splits input into parts, respecting double-quoted phrases.
// An unterminated quote returns ErrSyntax — silently auto-closing
// would let "find this and run rm -rf would be parsed as a phrase
// containing the trailing text, which is surprising.
func tokenize(input string) ([]part, error) {
	var (
		parts []part
		cur   strings.Builder
		inQ   bool
	)
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, part{body: cur.String(), quoted: false})
			cur.Reset()
		}
	}
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '"' && !inQ:
			flush()
			inQ = true
		case c == '"' && inQ:
			// FTS5 phrase-escape rule: a literal `"` inside a phrase
			// is written as two consecutive quotes. Treat `""` as a
			// single literal quote in the body rather than a close.
			if i+1 < len(input) && input[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			parts = append(parts, part{body: cur.String(), quoted: true})
			cur.Reset()
			inQ = false
		case unicode.IsSpace(rune(c)) && !inQ:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	if inQ {
		return nil, fmt.Errorf("%w: unclosed quote", ErrSyntax)
	}
	flush()
	return parts, nil
}

// transform converts one part to its FTS5 form.
//
//   - Quoted phrases become FTS5 phrases. Embedded `"` characters
//     (rare; only present if the user typed `"" inside their phrase)
//     are escaped by doubling, which is FTS5's quoting rule.
//   - Bare tokens that contain any FTS5 special or any of our
//     tokenizer separators become a phrase too — wrapping is the
//     simplest way to escape arbitrary punctuation without trying to
//     teach this package the FTS5 grammar.
//   - Pure word tokens get a trailing `*` so partial matches grow
//     naturally.
func transform(p part) (string, error) {
	if p.quoted {
		if p.body == "" {
			// "" — a no-op phrase. Skip rather than emit a degenerate
			// MATCH that FTS5 would reject.
			return "", nil
		}
		return `"` + escapeQuotes(p.body) + `"`, nil
	}
	if p.body == "" {
		return "", nil
	}
	if needsQuoting(p.body) {
		return `"` + escapeQuotes(p.body) + `"`, nil
	}
	return p.body + "*", nil
}

// needsQuoting reports whether a bare token must be wrapped in
// quotes — either because it contains FTS5 metacharacters or because
// it contains one of the tokenizer's separator characters (in which
// case the tokenizer would split it; wrapping makes it a phrase
// query of the resulting tokens, which is what the user wanted).
//
// Both ASCII and non-ASCII runes are checked: a Unicode quotation
// mark or a non-ASCII separator looks innocent to a byte scan but
// would still confuse the FTS5 parser if pasted into a MATCH
// expression unwrapped.
func needsQuoting(s string) bool {
	for _, r := range s {
		// Specials and separators are documented as ASCII; the
		// fast path is `r < MaxASCII && IndexByte`. For non-ASCII
		// runes, fall through to the strings.ContainsRune path so
		// any future expansion of the special set (e.g. a
		// non-ASCII quote-like char) is matched as well.
		if r <= unicode.MaxASCII {
			c := byte(r)
			if strings.IndexByte(fts5Specials, c) >= 0 {
				return true
			}
			if strings.IndexByte(separators, c) >= 0 {
				return true
			}
			continue
		}
		// Defensive: also check for Unicode characters classified
		// as quotation marks or other punctuation that an FTS5
		// parser might choke on. unicode.IsPunct is permissive,
		// but for safety we wrap any non-ASCII punctuation as a
		// phrase rather than letting it through as-is.
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return true
		}
	}
	return false
}

// escapeQuotes doubles any embedded `"` characters, the FTS5
// convention for a literal quote inside a quoted phrase.
func escapeQuotes(s string) string {
	if !strings.Contains(s, `"`) {
		return s
	}
	return strings.ReplaceAll(s, `"`, `""`)
}
