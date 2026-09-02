package events

import "testing"

// TestRenderToolContent_ApplyPatch covers Codex CLI's edit tool.
// Its tool_input is the whole patch envelope in a single `command`
// string; we render the touched paths so an edit is searchable by
// filename instead of by diff body.
func TestRenderToolContent_ApplyPatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		patch string
		want  string
	}{
		{
			name:  "add file",
			patch: "*** Begin Patch\n*** Add File: /tmp/work/foo.txt\n+bar\n*** End Patch",
			want:  "apply_patch /tmp/work/foo.txt",
		},
		{
			name: "multiple files keep source order",
			patch: "*** Begin Patch\n" +
				"*** Update File: internal/a.go\n@@\n-old\n+new\n" +
				"*** Delete File: internal/b.go\n" +
				"*** End Patch",
			want: "apply_patch internal/a.go internal/b.go",
		},
		{
			// A rename names its path twice — once to update the
			// contents, once to move it. Both are real, but the
			// duplicate isn't.
			name: "rename dedupes repeated paths",
			patch: "*** Begin Patch\n" +
				"*** Update File: old/name.go\n" +
				"*** Move to: new/name.go\n" +
				"*** Update File: old/name.go\n" +
				"*** End Patch",
			want: "apply_patch old/name.go new/name.go",
		},
		{
			// A hunk body line that merely starts with "***" is
			// not a header. Rendering it as a path would invent a
			// file the patch never touched.
			name: "patch body line starting with stars is not a header",
			patch: "*** Begin Patch\n" +
				"*** Update File: doc.md\n@@\n+*** Not A Header: nope\n" +
				"*** End Patch",
			want: "apply_patch doc.md",
		},
		{
			// Unparseable patch → bare tool name, never a guess.
			name:  "no recognisable headers falls back to bare name",
			patch: "some other tool's payload entirely",
			want:  "apply_patch",
		},
		{
			name:  "empty verb argument is skipped",
			patch: "*** Begin Patch\n*** Add File: \n*** End Patch",
			want:  "apply_patch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RenderToolContent(map[string]any{
				"tool_name":  "apply_patch",
				"tool_input": map[string]any{"command": tc.patch},
			})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderToolContent_CodexUsesClaudeToolNames pins the property
// the Codex integration leans on: Codex reports tools in Claude
// Code's PascalCase vocabulary with identically-named tool_input
// fields, so no Codex-specific aliases are needed here.
func TestRenderToolContent_CodexUsesClaudeToolNames(t *testing.T) {
	t.Parallel()
	got := RenderToolContent(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "cat note.txt"},
	})
	if want := "Bash cat note.txt"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
