package searchquery

import (
	"errors"
	"strings"
	"testing"
)

func TestToFTS5(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{
			name: "single bare word",
			in:   "mongo",
			want: "mongo*",
		},
		{
			name: "two bare words become AND of prefixes",
			in:   "mongo shutdown",
			want: "mongo* shutdown*",
		},
		{
			name: "leading and trailing whitespace ignored",
			in:   "   mongo   ",
			want: "mongo*",
		},
		{
			name: "multiple internal spaces collapse",
			in:   "mongo    shutdown",
			want: "mongo* shutdown*",
		},
		{
			name: "quoted phrase preserved",
			in:   `"mongo shutdown"`,
			want: `"mongo shutdown"`,
		},
		{
			name: "phrase mixed with bare tokens",
			in:   `error "panic stack"`,
			want: `error* "panic stack"`,
		},
		{
			name: "two phrases",
			in:   `"foo bar" "baz qux"`,
			want: `"foo bar" "baz qux"`,
		},
		{
			name: "dot-separated identifier wrapped as phrase",
			in:   "migrate.go",
			want: `"migrate.go"`,
		},
		{
			name: "slash-separated path wrapped as phrase",
			in:   "internal/store",
			want: `"internal/store"`,
		},
		{
			name: "underscore identifier wrapped as phrase",
			in:   "session_id",
			want: `"session_id"`,
		},
		{
			name: "dash identifier wrapped as phrase",
			in:   "claude-code",
			want: `"claude-code"`,
		},
		{
			name: "long path with multiple separators",
			in:   "internal/store/migrate.go",
			want: `"internal/store/migrate.go"`,
		},
		{
			name: "fts5 special star wrapped",
			in:   "foo*bar",
			want: `"foo*bar"`,
		},
		{
			name: "fts5 special paren wrapped",
			in:   "foo(bar",
			want: `"foo(bar"`,
		},
		{
			name: "embedded quote inside phrase escaped by doubling",
			in:   `"he said ""hi"" today"`,
			want: `"he said ""hi"" today"`,
		},
		{
			name: "non-ascii letters stay as prefix",
			in:   "мост",
			want: "мост*",
		},
		{
			name: "japanese stays as prefix",
			in:   "東京",
			want: "東京*",
		},
		{
			// Unicode quotation marks are punctuation; we wrap
			// them defensively rather than risk an FTS5 parse
			// error. Pinned by the A4 audit fix.
			name: "unicode left quote forces phrase wrapping",
			in:   "“hello",
			want: `"“hello"`,
		},
		{
			name: "unicode em-dash forces phrase wrapping",
			in:   "ab—cd",
			want: `"ab—cd"`,
		},
		{
			name:    "empty string errors",
			in:      "",
			wantErr: ErrEmpty,
		},
		{
			name:    "whitespace only errors",
			in:      "   \t\n  ",
			wantErr: ErrEmpty,
		},
		{
			name:    "unclosed quote errors",
			in:      `find "this without close`,
			wantErr: ErrSyntax,
		},
		{
			name: "empty quoted phrase is dropped",
			in:   `error ""`,
			want: "error*",
		},
		{
			name:    "only an empty phrase is empty",
			in:      `""`,
			wantErr: ErrEmpty,
		},
		{
			name: "uppercase preserved (FTS5 is case-insensitive)",
			in:   "MongoDB",
			want: "MongoDB*",
		},
		{
			name: "mixed phrases identifiers and words",
			in:   `fix migrate.go "panic in store"`,
			want: `fix* "migrate.go" "panic in store"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ToFTS5(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ToFTS5(%q): err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToFTS5(%q): unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ToFTS5(%q):\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestToFTS5_LongInput ensures pathological-length input doesn't
// blow up the parser. Not a correctness check — just don't allocate
// or loop badly.
func TestToFTS5_LongInput(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("word ", 1000)
	got, err := ToFTS5(in)
	if err != nil {
		t.Fatalf("ToFTS5: %v", err)
	}
	if !strings.HasSuffix(got, "word*") {
		t.Errorf("got %q (truncated): want trailing word*", got[:80])
	}
}

// TestToFTS5_ASCIIPunctuationIsQuoted is the regression gate for
// `aichronicles search "don't panic"` returning HTTP 500.
//
// needsQuoting was a denylist of characters with documented special
// meaning in FTS5 (`"():*^+`) plus our tokenizer's separators. But
// FTS5's bareword grammar is an allowlist — letters, digits, '_' and
// codepoints > 127 — so every other ASCII byte is a parse error. The
// unlisted ones fell through and got the prefix '*' appended, and
// SQLite answered with a syntax error.
//
// ToFTS5 returns nil error either way, so the bad expression flowed
// past the handler's "parse here so consumers get a clean 400"
// guard and surfaced as a generic 500 with an empty detail.
//
// The companion test in internal/store runs these through a real FTS5
// table; this one pins the translation itself.
func TestToFTS5_ASCIIPunctuationIsQuoted(t *testing.T) {
	t.Parallel()
	// Every ASCII punctuation mark that is not a bareword character.
	// '"' is excluded: it is the query language's own phrase
	// delimiter, so an unbalanced one is rejected up front with a
	// clean ErrSyntax rather than reaching FTS5.
	const punct = "',;:!?@#$%&=<>|\\~[]{}()*^+-./_`"
	for _, r := range punct {
		tok := "ab" + string(r) + "cd"
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			got, err := ToFTS5(tok)
			if err != nil {
				t.Fatalf("ToFTS5(%q): %v", tok, err)
			}
			if !strings.HasPrefix(got, `"`) {
				t.Errorf("ToFTS5(%q) = %q; token containing %q must be quoted, "+
					"otherwise FTS5 fails to parse it", tok, got, string(r))
			}
		})
	}
}

// TestToFTS5_KeywordsAreQuoted covers the other syntax-error class:
// FTS5 recognises AND/OR/NOT as operators in uppercase, and the
// prefix '*' appended to a bare operator is an outright parse error.
// The package doc promises operators are treated as content tokens.
func TestToFTS5_KeywordsAreQuoted(t *testing.T) {
	t.Parallel()
	for _, kw := range []string{"AND", "OR", "NOT"} {
		got, err := ToFTS5(kw)
		if err != nil {
			t.Fatalf("ToFTS5(%q): %v", kw, err)
		}
		if !strings.HasPrefix(got, `"`) {
			t.Errorf("ToFTS5(%q) = %q; must be quoted to stay a content token", kw, got)
		}
	}
	// Lowercase is not a keyword and stays a bare prefix term.
	got, err := ToFTS5("and")
	if err != nil {
		t.Fatalf("ToFTS5(and): %v", err)
	}
	if strings.HasPrefix(got, `"`) {
		t.Errorf("lowercase \"and\" should stay a bare term, got %q", got)
	}
}

// TestToFTS5_PlainWordsStayBare guards against over-quoting: the
// prefix-match behaviour users rely on must survive the stricter
// rule.
func TestToFTS5_PlainWordsStayBare(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"deploy", "Deploy2", "ünïcöde", "abc123"} {
		got, err := ToFTS5(in)
		if err != nil {
			t.Fatalf("ToFTS5(%q): %v", in, err)
		}
		if !strings.HasSuffix(got, "*") || strings.HasPrefix(got, `"`) {
			t.Errorf("ToFTS5(%q) = %q; plain words must stay bare prefix terms", in, got)
		}
	}
}
