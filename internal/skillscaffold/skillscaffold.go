// Package skillscaffold renders the SKILL.md (and helper-script)
// bytes that `aichronicles propose add` writes to disk. It is the
// single source of truth for that format so callers that only want
// to *preview* what `add` would produce — e.g. the web UI's
// /propose/{id}/{skill} page — render byte-identical output without
// importing internal/cli (which would be an import cycle: cli
// imports web).
//
// Frontmatter field names + omitempty match Claude Code's documented
// schema at https://code.claude.com/docs/en/skills#frontmatter-reference,
// with AutoSkill (Yang et al., 2026 — arXiv:2603.01145) skill-tuple
// metadata added as additional keys (YAML readers ignore unknown
// keys by spec; Claude Code's parser inherits that behaviour).
package skillscaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/textfmt"
)

// FrontmatterCharCap is the per-field ceiling we apply to the
// description and when_to_use scalars. Claude Code documents a
// 1536-char cap on the *combined* description + when_to_use text;
// we cap each field at well under half that so the combined block
// stays comfortably under the limit even when both are set.
const FrontmatterCharCap = 700

// FingerprintLen is the number of leading hex characters from the
// SHA-256 we embed in the SKILL.md provenance footer. 12 chars =
// 48 bits — plenty for human-readable cross-reference against the
// candidate row's add_body_sha256, while staying short enough to
// render compactly. The full hash lives in the DB; the fingerprint
// is just for visual identification.
const FingerprintLen = 12

// Frontmatter is what we yaml.Marshal at the top of SKILL.md.
//
//   - name:         lowercase letters / numbers / hyphens only,
//     ≤64 chars. Maps directly to the proposal's kebab-case name.
//   - description:  what the skill does and when. Front-loaded.
//   - when_to_use:  optional trigger phrases / example requests.
//   - version:      AutoSkill v — semver-ish; v0.1.0 on fresh add,
//     patch-bumped by every merge.
//   - kind:         contrastive-induction label ("pattern" /
//     "pitfall"); omitted when empty so legacy files aren't
//     retroactively annotated with a guessed value.
//   - tags:         AutoSkill γ — categorical labels for browsing.
//   - triggers:     AutoSkill τ — short query-shaped phrases that
//     activate retrieval. Distinct from when_to_use (prose).
//   - examples:     AutoSkill ξ — (input → output) demonstrations.
type Frontmatter struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description"`
	WhenToUse   string               `yaml:"when_to_use,omitempty"`
	Version     string               `yaml:"version,omitempty"`
	Kind        string               `yaml:"kind,omitempty"`
	Tags        []string             `yaml:"tags,omitempty"`
	Triggers    []string             `yaml:"triggers,omitempty"`
	Examples    []FrontmatterExample `yaml:"examples,omitempty"`
}

// FrontmatterExample is one (input, output) demonstration in the
// AutoSkill ξ set, rendered as a YAML object inside the frontmatter
// array.
type FrontmatterExample struct {
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
}

// FrontmatterKind normalises a contrastive-induction label for YAML
// frontmatter emission. Returns the input verbatim when it's a
// recognised value ("pattern" / "pitfall"), empty string otherwise.
// Mirrors the store-side normalisation so the SKILL.md frontmatter
// and the skill_candidates row never disagree on what the kind is.
func FrontmatterKind(s string) string {
	switch s {
	case "pattern", "pitfall":
		return s
	default:
		return ""
	}
}

// Rendered is the full output of rendering one proposed skill to
// SKILL.md bytes. Body is the scaffold without the provenance
// footer — it is what the SWE-Skills-Bench size budget is checked
// against and what SHA256 covers. Full is Body plus the provenance
// footer: the exact bytes `propose add` writes to disk.
type Rendered struct {
	Body   string // scaffold body, pre-footer
	SHA256 string // hex SHA-256 of Body
	Full   string // Body + ProvenanceFooter(SHA256)
}

// Render produces the SKILL.md content for sk exactly as
// `aichronicles propose add` writes it: the scaffold body, its
// SHA-256, and the body-plus-footer Full form. The hash is computed
// over the pre-footer body so the footer (which encodes the hash)
// stays reversible: a drift checker strips the trailing footer line,
// recomputes, and compares against skill_candidates.add_body_sha256.
func Render(sk *prompts.ProposedSkill, outputID int64) Rendered {
	body := renderBody(sk, outputID)
	sum := sha256.Sum256([]byte(body))
	hexsum := hex.EncodeToString(sum[:])
	return Rendered{
		Body:   body,
		SHA256: hexsum,
		Full:   body + ProvenanceFooter(hexsum),
	}
}

// ProvenanceFooter is the trailing block appended to a SKILL.md body
// after computing its hash. The line itself is not part of the hash
// (callers hash the body, then append), so a drift checker can strip
// the line, recompute, and compare against
// skill_candidates.add_body_sha256.
//
// SSGM (Lam et al., 2026 — arXiv:2603.11768) calls this primitive
// "consistency verification": the lifecycle index has to be able to
// tell "what aichronicles wrote" from "what was edited afterwards."
func ProvenanceFooter(bodySHA256 string) string {
	short := bodySHA256
	if len(short) > FingerprintLen {
		short = short[:FingerprintLen]
	}
	return fmt.Sprintf(
		"\n<!-- aichronicles-provenance: sha256:%s — drift check via "+
			"`aichronicles skills verify` (see skill_candidates.add_body_sha256). -->\n",
		short,
	)
}

// renderBody builds the SKILL.md body (frontmatter + markdown,
// without the provenance footer). Frontmatter is generated by
// yaml.v3 so quoting / escaping / line breaks are handled correctly
// without hand-rolled logic. The body deliberately mirrors the
// canonical examples in https://code.claude.com/docs/en/skills: a
// short intro paragraph and a single numbered Steps list. We do NOT
// invent section headers ("Pitfalls", "Verification") that aren't
// part of the documented format.
//
// Helper scripts are referenced inline as part of the Steps guidance
// (and in the trailing provenance footer) rather than in their own
// H2 section, matching the docs' "Reference supporting files from
// your SKILL.md so Claude knows what they contain" convention.
func renderBody(sk *prompts.ProposedSkill, outputID int64) string {
	examples := make([]FrontmatterExample, 0, len(sk.Examples))
	for _, e := range sk.Examples {
		examples = append(examples, FrontmatterExample{
			Input:  e.Input,
			Output: e.Output,
		})
	}
	frontmatter, err := yaml.Marshal(Frontmatter{
		Name:        sk.Name, // kebab-case verbatim from the proposal
		Description: textfmt.ClipToRunes(buildDescription(sk), FrontmatterCharCap),
		WhenToUse:   textfmt.ClipToRunes(strings.TrimSpace(sk.WhenToUse), FrontmatterCharCap),
		Version:     store.InitialSkillVersion, // fresh add — merge bumps
		Kind:        FrontmatterKind(sk.Kind),
		Tags:        append([]string(nil), sk.Tags...),
		Triggers:    append([]string(nil), sk.Triggers...),
		Examples:    examples,
	})
	if err != nil {
		// yaml.Marshal of a plain struct doesn't fail in practice;
		// fall back to a minimal frontmatter on the off chance it
		// does so we still produce a valid SKILL.md.
		frontmatter = []byte(fmt.Sprintf("name: %s\ndescription: %s\n", sk.Name, sk.WhenToUse))
	}

	var b strings.Builder
	fmt.Fprintln(&b, "---")
	b.Write(frontmatter)
	fmt.Fprintln(&b, "---")
	fmt.Fprintln(&b)

	// One-paragraph H1 intro: just enough to orient a reader who
	// invokes the skill via `/<name>` without recalling the
	// proposal context. We use why-text when present (it carries
	// the "what does this skill do" angle) and fall back to
	// when_to_use otherwise.
	intro := strings.TrimSpace(sk.Why)
	if intro == "" {
		intro = strings.TrimSpace(sk.WhenToUse)
	}
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", sk.Name, intro)

	// Steps — the procedural core. Single placeholder bullet
	// pointing at the evidence sessions; the user fills in the
	// real steps from those sessions. Inline references to any
	// helper scripts so the relationship between SKILL.md and
	// scripts/ is visible at a glance.
	fmt.Fprintln(&b, "## Steps")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "1. **TODO** — fill in from the evidence sessions in the footer. Capture")
	fmt.Fprintln(&b, "   the exact commands, file paths, and decision points so the next")
	fmt.Fprintln(&b, "   invocation replays them deterministically.")
	for _, sc := range sk.Scripts {
		fmt.Fprintf(&b, "   - Run `scripts/%s` to %s.\n",
			sc.Name, textfmt.LowerFirst(strings.TrimRight(strings.TrimSpace(sc.Purpose), ".")))
	}
	fmt.Fprintln(&b)

	// Provenance footer — kept compact. The skill body is loaded
	// into context every time the skill runs (per the docs'
	// content-lifecycle section), so a 50-row footer would burn
	// tokens for every invocation.
	fmt.Fprintln(&b, "---")
	fmt.Fprintf(&b, "*Scaffolded by `aichronicles propose add` from llm_outputs id=%d.*  \n", outputID)
	if sk.AlternativesRejected != "" {
		fmt.Fprintf(&b, "*Alternatives considered:* %s  \n", textfmt.OneLine(sk.AlternativesRejected))
	}
	if len(sk.Evidence) > 0 {
		fmt.Fprintln(&b, "*Evidence sessions:*")
		for _, ev := range sk.Evidence {
			fmt.Fprintf(&b, "- `%s` — %s\n", ev.SessionID, textfmt.OneLine(ev.WhatHappened))
		}
	}
	return b.String()
}

// buildDescription assembles the `description` frontmatter field.
// Per the docs, this is what Claude reads to decide when to load
// the skill — front-load the key use case, keep it concrete. We
// splice when_to_use's trigger into the same sentence so a single
// line covers "what" + "when" before truncation.
func buildDescription(sk *prompts.ProposedSkill) string {
	parts := []string{strings.TrimSpace(sk.Why), strings.TrimSpace(sk.WhenToUse)}
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, strings.TrimRight(p, "."))
		}
	}
	if len(out) == 0 {
		return sk.Name
	}
	return strings.Join(out, ". ") + "."
}

// RenderScript returns the body for one helper script under
// <skill>/scripts/<name>. Three populating paths, checked in
// priority order:
//
//  1. Steps[] (AWM-style parameterised template) — render each step
//     as a commented bash line, with a leading "Placeholders:"
//     doc-block listing each {token} the steps reference along with
//     its description and an example value.
//  2. Body (free-form starter the LLM grounded from evidence).
//     Dropped in verbatim under the header.
//  3. Neither — emit a TODO stub directing the user to fill in by
//     walking the cited sessions.
//
// The header always cites the originating skill and the proposal's
// llm_outputs id so provenance is greppable.
func RenderScript(sc *prompts.ProposedSkillScript, sk *prompts.ProposedSkill, outputID int64) string {
	var b strings.Builder
	fmt.Fprintln(&b, "#!/usr/bin/env bash")
	fmt.Fprintln(&b, "# "+strings.TrimSpace(sc.Purpose))
	fmt.Fprintln(&b, "#")
	fmt.Fprintf(&b, "# Skill: %s\n", sk.Name)
	fmt.Fprintf(&b, "# Scaffolded by `aichronicles propose add` from llm_outputs id=%d.\n", outputID)

	switch {
	case len(sc.Steps) > 0:
		WritePlaceholderBlock(&b, sc.Placeholders)
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "set -euo pipefail")
		fmt.Fprintln(&b)
		for _, step := range sc.Steps {
			if p := strings.TrimSpace(step.Purpose); p != "" {
				fmt.Fprintln(&b, "# "+p)
			}
			fmt.Fprintln(&b, step.Cmd)
			fmt.Fprintln(&b)
		}
	case strings.TrimSpace(sc.Body) != "":
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "set -euo pipefail")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, sc.Body)
	default:
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "set -euo pipefail")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "# TODO — replace this body with the actual implementation")
		fmt.Fprintln(&b, "# the evidence sessions repeat by hand.")
		fmt.Fprintln(&b, "echo 'TODO: implement' >&2")
		fmt.Fprintln(&b, "exit 1")
	}
	return b.String()
}

// WritePlaceholderBlock renders a leading comment block that
// documents each {token} the script's steps reference. Skipped
// entirely when no placeholders are present so a fully-concrete
// script doesn't get a confusing empty block. Exported so the
// merge path (cli.renderMergedScriptScaffold) shares the exact
// same placeholder rendering.
func WritePlaceholderBlock(b *strings.Builder, placeholders []prompts.ProposedScriptPlaceholder) {
	if len(placeholders) == 0 {
		return
	}
	fmt.Fprintln(b, "#")
	fmt.Fprintln(b, "# Placeholders (substitute before running):")
	for _, p := range placeholders {
		example := ""
		if strings.TrimSpace(p.Example) != "" {
			example = "  e.g. " + strings.TrimSpace(p.Example)
		}
		fmt.Fprintf(b, "#   {%s} — %s%s\n", p.Token, strings.TrimSpace(p.Description), example)
	}
}
