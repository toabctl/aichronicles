package textfmt

import "testing"

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
