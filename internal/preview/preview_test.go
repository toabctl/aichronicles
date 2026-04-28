package preview

import "testing"

func TestPick(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		topic    string
		prompt   string
		wantText string
		wantKind PreviewKind
	}{
		{"topic wins", "  add OAuth login  ", "/loop", "add OAuth login", KindTopic},
		{"prompt fallback", "", "implement the refresh-token rotation", "implement the refresh-token rotation", KindPrompt},
		{"muted on short prompt", "", "yes", MutedPlaceholder, KindMuted},
		{"muted on slash command", "", "/loop", MutedPlaceholder, KindMuted},
		{"muted on whitespace", "  \n\t  ", "  ", MutedPlaceholder, KindMuted},
	}
	for _, c := range cases {
		gotText, gotKind := Pick(c.topic, c.prompt)
		if gotText != c.wantText || gotKind != c.wantKind {
			t.Errorf("%s: got (%q, %s), want (%q, %s)", c.name, gotText, gotKind, c.wantText, c.wantKind)
		}
	}
}

func TestIsSubstantivePrompt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"yes", false},
		{"/loop", false},
		{"/loop fix the OAuth bug right now please", true}, // slash + space + ≥30 runes → real prompt
		{"this is exactly thirty chars!", false},
		{"this is exactly thirty chars!!", true}, // 30 runes
		{"implement the refresh-token rotation", true},
	}
	for _, c := range cases {
		if got := IsSubstantivePrompt(c.in); got != c.want {
			t.Errorf("IsSubstantivePrompt(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOneLine(t *testing.T) {
	t.Parallel()
	if got := OneLine(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := OneLine("a\nb\rc\td"); got != "a b c d" {
		t.Errorf("flatten: got %q", got)
	}
	long := ""
	for i := 0; i < MaxOneLineRunes+10; i++ {
		long += "x"
	}
	got := OneLine(long)
	if len([]rune(got)) != MaxOneLineRunes+1 { // MaxOneLineRunes runes + ellipsis
		t.Errorf("truncate length: got %d runes, want %d", len([]rune(got)), MaxOneLineRunes+1)
	}
	if got[len(got)-len("…"):] != "…" {
		t.Errorf("ellipsis missing: got %q", got)
	}
}
