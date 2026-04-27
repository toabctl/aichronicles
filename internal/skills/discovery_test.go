package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

func writeSkill(t *testing.T, dir, name, frontmatter string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(frontmatter), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// TestReadSkillDescription_ParsesQuotedAndUnquoted covers the
// shapes seen in real ~/.claude/skills/ files: double-quoted,
// single-quoted, unquoted bare strings.
func TestReadSkillDescription_ParsesQuotedAndUnquoted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "double-quoted",
			body: "---\nname: foo\ndescription: \"a quoted desc\"\n---\n# body\n",
			want: "a quoted desc",
		},
		{
			name: "single-quoted",
			body: "---\nname: foo\ndescription: 'single desc'\n---\n",
			want: "single desc",
		},
		{
			name: "unquoted",
			body: "---\nname: foo\ndescription: bare string here\n---\n",
			want: "bare string here",
		},
		{
			name: "no description field",
			body: "---\nname: foo\n---\n",
			want: "",
		},
		{
			name: "no opening fence",
			body: "name: foo\ndescription: nope\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, _ := ReadDescription(path)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScanSkillsDir_UsesDirectoryNameAsCanonical confirms we
// surface the directory name (which matches Skill.skill payload
// values) rather than the human-readable YAML name field. Real
// example: dir "effective-go" / yaml name "Effective Go".
func TestScanSkillsDir_UsesDirectoryNameAsCanonical(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkill(t, dir, "effective-go",
		"---\nname: Effective Go\ndescription: \"idiomatic Go\"\n---\n")
	writeSkill(t, dir, "build-test",
		"---\ndescription: build then test\n---\n")
	// Junk dirs the scanner must skip.
	if err := os.MkdirAll(filepath.Join(dir, "no-skill-md"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := ScanDir(dir, "test-source")
	want := []prompts.InstalledSkill{
		{Name: "build-test", Description: "build then test", Source: "test-source"},
		{Name: "effective-go", Description: "idiomatic Go", Source: "test-source"},
	}
	// ScanDir doesn't sort — sort here for deterministic
	// comparison; alphabetisation happens in collectInstalledSkills.
	sortByName(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestFindProjectSkillsRoot_WalksUpFromCwd pins the requirement
// that a session whose start cwd is *inside* a project (not at
// the project root) still resolves to the right .claude/skills/
// directory. This is the common case: cwd=/proj/internal/cli
// while .claude/skills lives at /proj/.claude/skills.
func TestFindProjectSkillsRoot_WalksUpFromCwd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	deep := filepath.Join(proj, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(proj, ClaudeSkillsDirName), 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}

	if got := FindProjectRoot(deep); got != proj {
		t.Errorf("deep cwd: got %q, want %q", got, proj)
	}
	if got := FindProjectRoot(proj); got != proj {
		t.Errorf("at-root cwd: got %q, want %q", got, proj)
	}
	if got := FindProjectRoot(root); got != "" {
		t.Errorf("no-skills ancestor: got %q, want empty", got)
	}
	if got := FindProjectRoot(""); got != "" {
		t.Errorf("empty cwd: got %q, want empty", got)
	}
}

// sortByName is a small in-package sort helper to keep
// ScanDir tests deterministic without exporting a sort.
func sortByName(xs []prompts.InstalledSkill) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1].Name > xs[j].Name; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
