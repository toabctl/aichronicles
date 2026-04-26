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
