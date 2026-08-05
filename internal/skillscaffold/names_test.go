package skillscaffold

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateSkillName_RejectsTraversal is the security gate. Skill
// names arrive as free-form strings in an LLM tool call and are then
// joined onto a filesystem path with filepath.Join, which RESOLVES
// ".." rather than rejecting it — so an unvalidated name escapes the
// skills tree entirely.
//
// The JSON Schema constrains the field, but a schema is a hint to the
// provider, not an enforcement point; the prompt layer already
// documents that bad shapes reach our decode layer. And because
// transcript content is attacker-influenceable, the model can be
// steered into emitting one of these.
func TestValidateSkillName_RejectsTraversal(t *testing.T) {
	t.Parallel()
	bad := []struct {
		name  string
		value string
	}{
		{"parent segment", ".."},
		{"current segment", "."},
		{"embedded traversal", "x/../../../../.config/systemd/user/evil"},
		{"leading slash", "/etc/passwd"},
		{"trailing slash", "deploy/"},
		{"nested path", "a/b"},
		{"absolute-ish", "//etc"},
		{"dash prefix", "-rf"},
		{"nul byte", "deploy\x00evil"},
		{"empty", ""},
		{"uppercase", "Deploy"},
		{"underscore", "deploy_skill"},
		{"space", "deploy skill"},
		{"leading digit", "1deploy"},
		{"too long", strings.Repeat("a", MaxSkillNameLen+1)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateSkillName(tc.value); err == nil {
				t.Errorf("ValidateSkillName(%q) accepted a name that must be rejected", tc.value)
			}
		})
	}
}

func TestValidateSkillName_AcceptsRealNames(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{
		"deploy",
		"deploy-staging",
		"git-repos",
		"a",
		"cg-image-review",
		strings.Repeat("a", MaxSkillNameLen),
	} {
		if err := ValidateSkillName(ok); err != nil {
			t.Errorf("ValidateSkillName(%q) rejected a legitimate name: %v", ok, err)
		}
	}
}

// TestValidateScriptName_RejectsTraversal covers the helper-script
// half. These are written mode 0755, so an escape here plants an
// executable at an arbitrary path.
//
// Note the tool schema's character class (^[A-Za-z0-9_.-]+$) admits
// ".." on its own, which is exactly why the dot-segment rule is
// enforced separately rather than left to the regex.
func TestValidateScriptName_RejectsTraversal(t *testing.T) {
	t.Parallel()
	bad := []string{
		"..",
		".",
		"../../evil.sh",
		"/tmp/evil.sh",
		"sub/dir.sh",
		"-rf.sh",
		"evil\x00.sh",
		"",
		"has space.sh",
		strings.Repeat("a", MaxScriptNameLen+1),
	}
	for _, v := range bad {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if err := ValidateScriptName(v); err == nil {
				t.Errorf("ValidateScriptName(%q) accepted a name that must be rejected", v)
			}
		})
	}
}

func TestValidateScriptName_AcceptsRealNames(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"build-test.sh", "run.py", "a", "deploy_v2.sh", "x.y.z"} {
		if err := ValidateScriptName(ok); err != nil {
			t.Errorf("ValidateScriptName(%q) rejected a legitimate name: %v", ok, err)
		}
	}
}

// TestValidatedNamesCannotEscapeRoot is the property the individual
// rules exist to produce, asserted directly against filepath.Join so
// it holds no matter how the grammar evolves.
func TestValidatedNamesCannotEscapeRoot(t *testing.T) {
	t.Parallel()
	const root = "/home/user/.claude/skills"
	candidates := []string{
		"..", "../evil", "x/../../../etc", "/etc/passwd", "deploy",
		"deploy-staging", "a/b", ".", "-rf",
	}
	for _, name := range candidates {
		if err := ValidateSkillName(name); err != nil {
			continue // rejected, which is the desired outcome
		}
		joined := filepath.Join(root, name)
		if !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			t.Errorf("ValidateSkillName accepted %q but filepath.Join escapes root: %q", name, joined)
		}
	}
}

func TestValidateProposedSkillNames(t *testing.T) {
	t.Parallel()

	t.Run("accepts a well-formed set", func(t *testing.T) {
		t.Parallel()
		if err := ValidateProposedSkillNames("deploy", []string{"a.sh", "b.sh"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("rejects on the first bad script", func(t *testing.T) {
		t.Parallel()
		err := ValidateProposedSkillNames("deploy", []string{"ok.sh", "../evil.sh"})
		if err == nil {
			t.Fatal("expected an error for a traversing script name")
		}
		if !strings.Contains(err.Error(), "evil.sh") {
			t.Errorf("error should name the offending script, got: %v", err)
		}
	})

	t.Run("rejects duplicate script names", func(t *testing.T) {
		t.Parallel()
		// Two scripts writing the same path means the second
		// silently clobbers the first.
		if err := ValidateProposedSkillNames("deploy", []string{"a.sh", "a.sh"}); err == nil {
			t.Error("expected an error for duplicate script names")
		}
	})

	t.Run("rejects a bad skill name before looking at scripts", func(t *testing.T) {
		t.Parallel()
		err := ValidateProposedSkillNames("../evil", []string{"a.sh"})
		if err == nil || !strings.Contains(err.Error(), "skill name") {
			t.Errorf("expected a skill-name error, got: %v", err)
		}
	})
}
