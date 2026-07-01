package eval

import (
	"reflect"
	"testing"
)

func TestSubstringGrounder(t *testing.T) {
	t.Parallel()
	g := SubstringGrounder([]string{"ran `go test ./...` and it passed", "used the --since flag"})
	cases := []struct {
		atom string
		want bool
	}{
		{"go test", true},      // substring of first quote
		{"GO TEST", true},      // case-insensitive
		{"  --since  ", true},  // trimmed
		{"npm install", false}, // absent
		{"", false},            // empty never grounded
		{"   ", false},         // whitespace-only never grounded
	}
	for _, tc := range cases {
		if got := g(tc.atom); got != tc.want {
			t.Errorf("grounded(%q) = %v, want %v", tc.atom, got, tc.want)
		}
	}
}

func TestMembershipGrounder(t *testing.T) {
	t.Parallel()
	g := MembershipGrounder([]string{"sess-a", " sess-b ", ""})
	cases := []struct {
		atom string
		want bool
	}{
		{"sess-a", true},
		{"sess-b", true},         // allow-list entry was trimmed
		{" sess-a ", true},       // atom is trimmed
		{"sess-a-longer", false}, // exact match only, not substring
		{"sess-c", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := g(tc.atom); got != tc.want {
			t.Errorf("member(%q) = %v, want %v", tc.atom, got, tc.want)
		}
	}
}

func TestFabrication(t *testing.T) {
	t.Parallel()
	corpus := []string{"the migration adds an index on events.session_id"}
	g := SubstringGrounder(corpus)

	rep := Fabrication([]string{"index on events", "invented claim", "  ", "session_id"}, g)
	if rep.Total != 3 { // the whitespace-only atom is ignored
		t.Errorf("Total = %d, want 3", rep.Total)
	}
	if want := []string{"invented claim"}; !reflect.DeepEqual(rep.Ungrounded, want) {
		t.Errorf("Ungrounded = %v, want %v", rep.Ungrounded, want)
	}
	if got := rep.Rate(); got != 1.0/3.0 {
		t.Errorf("Rate = %v, want %v", got, 1.0/3.0)
	}
	if rep.Clean() {
		t.Error("Clean() = true, want false (one atom fabricated)")
	}
}

func TestFabrication_EmptyIsClean(t *testing.T) {
	t.Parallel()
	rep := Fabrication(nil, SubstringGrounder(nil))
	if rep.Total != 0 || !rep.Clean() || rep.Rate() != 0 {
		t.Errorf("empty report: total=%d clean=%v rate=%v; want 0,true,0", rep.Total, rep.Clean(), rep.Rate())
	}
}

func TestFabrication_AllGroundedIsClean(t *testing.T) {
	t.Parallel()
	g := SubstringGrounder([]string{"alpha beta gamma"})
	rep := Fabrication([]string{"alpha", "beta"}, g)
	if !rep.Clean() || rep.Rate() != 0 || rep.Total != 2 {
		t.Errorf("all-grounded: clean=%v rate=%v total=%d", rep.Clean(), rep.Rate(), rep.Total)
	}
}
