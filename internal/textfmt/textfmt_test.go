package textfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCollapseWhitespace(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"hello world":          "hello world",
		"hello\nworld":         "hello world",
		"  hello   \n\nworld ": " hello world ",
		"single":               "single",
	}
	for in, want := range cases {
		if got := CollapseWhitespace(in); got != want {
			t.Errorf("CollapseWhitespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOneLine(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"  hello   \n\nworld ": "hello world",
		"\tsingle\n":           "single",
		"":                     "",
	}
	for in, want := range cases {
		if got := OneLine(in); got != want {
			t.Errorf("OneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOneLineN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 80, "short"},
		{"multi\nline   value", 80, "multi line value"},
		{"abcdefghij", 5, "abcd…"},
	}
	for _, tc := range cases {
		if got := OneLineN(tc.in, tc.n); got != tc.want {
			t.Errorf("OneLineN(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestClipToRunes_LandsOnWordBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "short string passes through", in: "hello", max: 100, want: "hello"},
		{name: "clip at word boundary", in: "hello world how are you", max: 12,
			want: "hello world…"},
		{name: "no boundary near end", in: "supercalifragilisticexpialidocious", max: 10,
			want: "supercalif…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClipToRunes(tc.in, tc.max); got != tc.want {
				t.Errorf("ClipToRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestLowerFirst(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Hello world": "hello world",
		"hello world": "hello world",
		"":            "",
		"X":           "x",
	}
	for in, want := range cases {
		if got := LowerFirst(in); got != want {
			t.Errorf("LowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClipToRunes_WordBoundaryUsesRuneCounts pins the guard that
// decides whether to break at a word boundary.
//
// LastIndexAny returns a BYTE index, and it was compared against
// max/2, a RUNE count. For multi-byte text the byte index runs up to
// 3x the rune index, so the "only break past the halfway mark" test
// passed for boundaries far earlier than intended and the result came
// back much shorter than max allows. The slice itself was safe — the
// boundary characters are ASCII — only the length was wrong.
func TestClipToRunes_WordBoundaryUsesRuneCounts(t *testing.T) {
	t.Parallel()
	// Each 'ü' is two bytes, so the byte index runs at ~2x the rune
	// index. The sole space sits at rune 6 (byte 12). Against max=20
	// the halfway mark is 10, so:
	//
	//   byte index 12 > 10  → the old guard accepted it and clipped
	//                          the result down to 6 runes
	//   rune index  6 !> 10 → the fixed guard rejects it and uses the
	//                          full 20-rune budget
	//
	// Any input where those two disagree exercises the bug; this is
	// the smallest one.
	in := "üüüüüü üüüüüüüüüüüüüü"
	got := ClipToRunes(in, 20)
	body := strings.TrimSuffix(got, "…")

	// With a byte-index comparison the sole space (byte 8, rune 4)
	// cleared the `> max/2` test and the result collapsed to "üüüü…".
	// Comparing rune counts rejects it, so the full budget is used.
	if n := utf8.RuneCountInString(body); n < 10 {
		t.Errorf("clipped at an early word boundary the rune guard should reject: "+
			"%d runes, want close to 20: %q", n, got)
	}
}

// TestClipToRunes_NeverSplitsARune is the property that matters most:
// a byte slice through a multi-byte rune emits U+FFFD.
func TestClipToRunes_NeverSplitsARune(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		strings.Repeat("→", 50),
		strings.Repeat("ü", 50),
		"go test ./... → ≥90% coverage " + strings.Repeat("é", 40),
		strings.Repeat("🎉", 30),
	} {
		for _, max := range []int{1, 5, 10, 25, 49, 50, 100} {
			got := ClipToRunes(in, max)
			if !utf8.ValidString(got) {
				t.Errorf("ClipToRunes(%q, %d) produced invalid UTF-8: %q", in, max, got)
			}
			if strings.ContainsRune(got, '�') {
				t.Errorf("ClipToRunes(%q, %d) split a rune: %q", in, max, got)
			}
		}
	}
}
