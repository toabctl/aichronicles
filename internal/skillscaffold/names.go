package skillscaffold

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Skill and helper-script names arrive as free-form strings in an LLM
// tool call and are then joined onto a filesystem path. Nothing else
// stands between the model and os.WriteFile, so they are validated
// here.
//
// The JSON Schema shipped with the tool constrains both fields
// (^[a-z][a-z0-9-]*$ for skills, ^[A-Za-z0-9_.-]+$ for scripts), but a
// schema is a hint to the provider, not an enforcement point. The
// prompt layer already documents that Anthropic's tool-input
// validation is permissive about type-vs-schema mismatches, so the
// bad shape arrives at our decode layer rather than being rejected
// upstream — which means the grammar has to be checked locally too.
//
// This matters because transcript content is attacker-influenceable:
// a fetched page or a pasted issue body can steer a proposal, and the
// resulting name is joined with filepath.Join, which resolves ".."
// rather than rejecting it. A name of
// "x/../../../../.config/systemd/user/evil.service" escapes the
// skills tree entirely, and helper scripts are written mode 0755.
//
// Rejecting is the right response, not sanitising: a name we had to
// rewrite is a name the model did not actually choose, and silently
// installing a skill under a different name than the one shown to the
// user is its own bug.

var (
	// skillNameRE mirrors the tool schema exactly.
	skillNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// scriptNameRE mirrors the tool schema, minus the "no dot
	// segments" rule enforced separately below — the schema's
	// character class admits ".." on its own.
	scriptNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// MaxSkillNameLen bounds a skill directory name. Generous relative to
// real names (the longest shipped skill is ~30 chars) but well under
// every filesystem's per-component limit.
const MaxSkillNameLen = 64

// MaxScriptNameLen matches the tool schema's maxLength.
const MaxScriptNameLen = 64

// ValidateSkillName reports whether name is safe to use as a skill
// directory component. Returns a descriptive error naming the rule
// that failed, so an operator can tell a malformed proposal from a
// hostile one.
func ValidateSkillName(name string) error {
	if err := validatePathComponent("skill name", name, MaxSkillNameLen); err != nil {
		return err
	}
	if !skillNameRE.MatchString(name) {
		return fmt.Errorf(
			"skill name %q must be kebab-case matching %s", name, skillNameRE)
	}
	return nil
}

// ValidateScriptName reports whether name is safe to use as a helper
// script filename inside a skill's scripts/ directory.
func ValidateScriptName(name string) error {
	if err := validatePathComponent("script name", name, MaxScriptNameLen); err != nil {
		return err
	}
	if !scriptNameRE.MatchString(name) {
		return fmt.Errorf(
			"script name %q must match %s", name, scriptNameRE)
	}
	return nil
}

// validatePathComponent enforces the rules that matter regardless of
// which grammar applies: the value must be exactly one non-special
// path component.
//
// Checked before the regex so traversal attempts get a precise error
// rather than a generic "does not match pattern".
func validatePathComponent(what, name string, maxLen int) error {
	if name == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if len(name) > maxLen {
		return fmt.Errorf("%s is %d bytes, over the %d limit", what, len(name), maxLen)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%s %q is a path traversal segment", what, name)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("%s %q must be a single path component, not a path", what, name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%s %q contains a NUL byte", what, name)
	}
	// A leading dash turns the name into an option when it reaches
	// the follow-up commands we print for the user to paste.
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%s %q must not start with a dash", what, name)
	}
	// Defence in depth: whatever the grammar allows, the cleaned form
	// must still be the same single component.
	if filepath.Clean(name) != name || filepath.Base(name) != name {
		return fmt.Errorf("%s %q is not a plain path component", what, name)
	}
	return nil
}

// ValidateProposedSkillNames checks a skill name together with every
// helper-script name that will be written beneath it, so a caller
// gets one verdict for the whole write rather than discovering the
// second problem after the first file has landed.
func ValidateProposedSkillNames(skillName string, scriptNames []string) error {
	if err := ValidateSkillName(skillName); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(scriptNames))
	for _, n := range scriptNames {
		if err := ValidateScriptName(n); err != nil {
			return err
		}
		if _, dup := seen[n]; dup {
			return fmt.Errorf("duplicate script name %q", n)
		}
		seen[n] = struct{}{}
	}
	return nil
}
