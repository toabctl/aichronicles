package resumecmd

import (
	"reflect"
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
