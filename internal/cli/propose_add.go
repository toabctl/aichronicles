package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// newProposeAddCmd is the AutoSkill (Yang et al., 2026 —
// arXiv:2603.01145) maintenance action 'add' command: materialise
// one skill from the latest cached `propose` output as on-disk
// files. Writes the SKILL.md plus any helper scripts the proposal
// carried (skill-scoped, under <skill-dir>/scripts/<name>) — the
// canonical locations Claude Code reads from.
//
// Closes the suggestion → action loop: today `propose` outputs JSON
// saying "you should make a skill called X" and the user has to
// manually create the directory + frontmatter. After this command,
// the scaffold lands one keystroke away with the proposal's
// evidence embedded as guidance for the user to fill in.
//
// One artefact per pattern by design: scripts live inside the
// skill they belong to (matches Claude Code's skill format and
// hermes-agent's `skill_manage write_file` model). Practice
// invariants without a trigger condition (CLAUDE.md territory)
// are explicitly out of scope for propose, so there's no
// `--rule` path here.
func newProposeAddCmd() *cobra.Command {
	var (
		dbPath    string
		skillName string
		outputID  int64
		skillsDir string
		force     bool
		noVerify  bool
	)
	cmd := &cobra.Command{
		Use:   "add --skill <name>",
		Short: "Add a proposed skill (SKILL.md + scripts) to disk (AutoSkill action 'add')",
		Long: "Loads the latest cached `propose` output (or the one\n" +
			"identified by --output-id) and writes the named skill to\n" +
			"~/.claude/skills/<name>/. Includes:\n\n" +
			"  - SKILL.md with frontmatter (name, description, version,\n" +
			"    tags, triggers, examples — the AutoSkill 7-tuple) and a\n" +
			"    scaffolded body (When to use, Steps with TODO markers).\n" +
			"  - scripts/<name> for each helper script the proposal\n" +
			"    listed under the skill (chmod 0755, with shebang and\n" +
			"    purpose-comment header).\n\n" +
			"Verification gate (Voyager-style critic): before writing,\n" +
			"a second LLM pass evaluates the proposed skill against its\n" +
			"cited evidence and your installed skills. On a refusal\n" +
			"(near-duplicate of an installed skill, evidence too thin,\n" +
			"generic when_to_use, or fabricated steps) the add is\n" +
			"aborted with the critic's concern + recommendation. Pass\n" +
			"--no-verify to bypass the gate. The verification result is\n" +
			"cached as kind=propose_verify so re-running on the same\n" +
			"proposal is free.\n\n" +
			"All targets are refused if they already exist unless\n" +
			"--force is passed. Use `aichronicles propose list` to see\n" +
			"what's in the cached proposal. Use `aichronicles propose\n" +
			"merge --skill <name>` instead to fold the candidate into\n" +
			"an existing on-disk skill rather than creating a new one.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			if skillName == "" {
				return errors.New("--skill <name> is required (run `aichronicles propose list` to see options)")
			}

			result, output, err := loadLatestProposal(cmd.Context(), s, outputID)
			if err != nil {
				return err
			}

			// Lazy client + cfg: only constructed when verify
			// fires. --no-verify skips it entirely so users can
			// apply offline / without an API key.
			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				return cfgErr
			}
			llmCfg := LLMConfigFromFile(cfg.LLM)
			ctx, cancel := context.WithTimeout(cmd.Context(),
				cfg.Limits.ReflectTimeout.Or(defaultMetaLLMTimeout))
			defer cancel()
			newClient := func() (llm.Client, error) {
				return llm.FromConfig(ctx, llmCfg)
			}

			return addSkillCandidate(ctx, s, result, output.ID, skillName,
				resolveSkillsDir(skillsDir), force, noVerify,
				newClient, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().StringVar(&skillName, "skill", "",
		"name of a skill from the proposal to materialise")
	cmd.Flags().Int64Var(&outputID, "output-id", 0,
		"specific llm_outputs row id (default: latest propose row)")
	cmd.Flags().StringVar(&skillsDir, "skills-dir", "",
		"override target directory (default: ~/.claude/skills)")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite existing target files")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false,
		"skip the critic-LLM verification gate (writes the SKILL without checking for duplicates / weak evidence)")
	return cmd
}

// newProposeListCmd shows what's available in the latest cached
// proposal so the user can pick a name for `apply`. Pure read;
// never touches the filesystem.
func newProposeListCmd() *cobra.Command {
	var (
		dbPath   string
		outputID int64
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List skills in the latest cached propose output",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			result, output, err := loadLatestProposal(cmd.Context(), s, outputID)
			if err != nil {
				return err
			}
			renderProposalIndex(cmd.OutOrStdout(), result, output)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().Int64Var(&outputID, "output-id", 0,
		"specific llm_outputs row id (default: latest propose row)")
	return cmd
}

// loadLatestProposal returns the parsed ProposalResult plus the
// underlying llm_outputs row. When wantID > 0 the matching row is
// loaded directly; otherwise we return the most recent
// kind=propose row. Both paths produce a clear error when no
// proposal has been generated yet.
func loadLatestProposal(ctx context.Context, s *store.Store, wantID int64) (*prompts.ProposalResult, *store.LLMOutput, error) {
	var output *store.LLMOutput
	if wantID > 0 {
		out, err := store.LoadLLMOutputByID(ctx, s.DB(), wantID)
		if err != nil {
			return nil, nil, fmt.Errorf("load propose output id=%d: %w", wantID, err)
		}
		if out == nil || out.Kind != store.LLMKindPropose {
			return nil, nil, fmt.Errorf("llm_outputs id=%d is not a propose row", wantID)
		}
		output = out
	} else {
		rows, err := store.LoadLLMOutputs(ctx, s.DB(), store.LLMOutputFilter{
			Kind:  store.LLMKindPropose,
			Limit: 1,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("load latest propose: %w", err)
		}
		if len(rows) == 0 {
			return nil, nil, errors.New("no cached propose output found — run `aichronicles propose` first")
		}
		output = &rows[0]
	}

	var result prompts.ProposalResult
	if err := json.Unmarshal([]byte(output.Body), &result); err != nil {
		return nil, nil, fmt.Errorf("parse cached propose body (id=%d): %w", output.ID, err)
	}
	return &result, output, nil
}

// renderProposalIndex prints a short tabular view of every skill in
// the proposal — its position, its name, evidence count, effort,
// and a one-line trigger preview. Output is plain text so a user
// can pipe it through grep / awk.
func renderProposalIndex(out io.Writer, r *prompts.ProposalResult, output *store.LLMOutput) {
	_, _ = fmt.Fprintf(out, "propose output id=%d  generated=%s  model=%s\n\n",
		output.ID,
		time.UnixMilli(output.CreatedAtMs).UTC().Format("2006-01-02 15:04 UTC"),
		output.Model,
	)
	if len(r.Skills) == 0 {
		_, _ = fmt.Fprintln(out, "(no skills in proposal)")
		return
	}
	_, _ = fmt.Fprintf(out, "Skills (%d):\n", len(r.Skills))
	for i, sk := range r.Skills {
		_, _ = fmt.Fprintf(out, "  [%d] %-30s evidence=%d  effort=%s  scripts=%d\n",
			i+1, sk.Name, len(sk.Evidence), sk.Effort, len(sk.Scripts))
		_, _ = fmt.Fprintf(out, "      %s\n", oneLine(sk.WhenToUse))
		for _, sc := range sk.Scripts {
			_, _ = fmt.Fprintf(out, "      └── scripts/%s  — %s\n", sc.Name, oneLineN(sc.Purpose, 80))
		}
	}
	_, _ = fmt.Fprintln(out, "  add:    aichronicles propose add --skill <name>")
}

// addSkillCandidate writes ~/.claude/skills/<name>/SKILL.md plus
// any scripts the proposal carried under
// ~/.claude/skills/<name>/scripts/<script-name>. The agent owns
// further editing — we leave "TODO" markers in the procedural
// sections (Steps, Pitfalls, Verification) the user has to fill
// in from the cited evidence.
//
// Before writing, runs the Voyager-style critic gate via
// verifyProposalOrAbort (unless noVerify is set). On a refusal the
// function returns the critic's concern as an error and writes
// nothing to disk — propose's evidence is the substrate the gate
// checks against, and writing-then-rolling-back would risk leaving
// a half-skill on disk.
func addSkillCandidate(
	ctx context.Context,
	st *store.Store,
	r *prompts.ProposalResult,
	outputID int64,
	name, root string,
	force, noVerify bool,
	newClient func() (llm.Client, error),
	out io.Writer,
) error {
	sk, err := findProposedSkill(r, name)
	if err != nil {
		return err
	}

	dir := filepath.Join(root, sk.Name)
	skillMd := filepath.Join(dir, "SKILL.md")

	// Dedup-against-installed runs BEFORE the verify gate: the
	// critic LLM is the expensive call, so a same-name collision
	// is the cheapest possible refusal. The error message points
	// the user at `propose merge` so they don't reach for --force
	// out of habit when "merge into the existing skill" is what
	// they actually want.
	if err := refuseDuplicateSkillName(skillMd, sk.Name, force); err != nil {
		return err
	}

	if !noVerify {
		if err := verifyProposalOrAbort(ctx, st, sk, outputID, newClient, out); err != nil {
			return err
		}
	}

	for _, sc := range sk.Scripts {
		if err := refuseExistingUnlessForce(filepath.Join(dir, "scripts", sc.Name), force); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensure %s: %w", dir, err)
	}
	body := renderSkillScaffold(sk, outputID)
	if err := refuseOversizedSkill(skillMd, body, force); err != nil {
		return err
	}
	// SSGM-style provenance hash on the body BEFORE we append the
	// trailing provenance line. The line itself is computed from
	// the hash, so to keep the relationship reversible we hash the
	// pre-provenance body, then append. A drift checker can later
	// strip the trailing provenance line, recompute, and compare.
	bodyHash := sha256.Sum256([]byte(body))
	bodyHashHex := hex.EncodeToString(bodyHash[:])
	body += skillProvenanceFooter(bodyHashHex)
	if err := os.WriteFile(skillMd, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", skillMd, err)
	}
	_, _ = fmt.Fprintf(out, "wrote %s\n", skillMd)

	if len(sk.Scripts) > 0 {
		scriptsDir := filepath.Join(dir, "scripts")
		if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
			return fmt.Errorf("ensure %s: %w", scriptsDir, err)
		}
		for _, sc := range sk.Scripts {
			target := filepath.Join(scriptsDir, sc.Name)
			scriptBody := renderSkillScriptScaffold(&sc, sk, outputID)
			if err := os.WriteFile(target, []byte(scriptBody), 0o755); err != nil {
				return fmt.Errorf("write %s: %w", target, err)
			}
			_, _ = fmt.Fprintf(out, "wrote %s (executable)\n", target)
		}
	}

	// Lifecycle tracking: the SKILL.md (and any scripts) landed —
	// record AutoSkill maintenance action 'add' on the candidate
	// row so this skill on disk is attributable back to this
	// propose / induction run.
	//
	// Best-effort: a missing row (the candidate predates the
	// skill_candidates migration, or was extracted via a path that
	// doesn't seed the index) downgrades to a fresh insert + add,
	// so the lifecycle index converges to truth. All other errors
	// are logged and the apply is reported as successful — the
	// SKILL.md is on disk regardless.
	now := time.Now().UnixMilli()
	if merr := store.MarkSkillCandidateAddedWithProvenance(ctx, st.DB(),
		outputID, sk.Name, skillMd, now, bodyHashHex); merr != nil {
		if errors.Is(merr, store.ErrSkillCandidateNotFound) {
			if rerr := store.RecordSkillCandidate(ctx, st.DB(), outputID, sk.Name, now); rerr == nil {
				_ = store.MarkSkillCandidateAddedWithProvenance(ctx, st.DB(),
					outputID, sk.Name, skillMd, now, bodyHashHex)
			}
		} else {
			_, _ = fmt.Fprintf(out, "warning: failed to record skill lifecycle: %v\n", merr)
		}
	}

	_, _ = fmt.Fprintf(out, "\nNext: open the SKILL.md and fill in the Steps section\n")
	_, _ = fmt.Fprintf(out, "from these recurring sessions:\n")
	for _, ev := range sk.Evidence {
		_, _ = fmt.Fprintf(out, "  • aichronicles sessions --session %s\n", ev.SessionID)
	}
	return nil
}

// skillMdBudgetRunes caps the size of a freshly-added SKILL.md.
// SWE-Skills-Bench (Han et al., 2026 — arXiv:2603.15401) found that
// token overhead of induced skills ranges from −78% to +451% of
// baseline session size, decoupled from any pass-rate gain — i.e.
// long skills routinely cost a lot without helping. The Claude
// skills marketplace (Ling et al., 2026 — arXiv:2602.08004) reports
// a median of 1,414 tokens per skill, with a heavy tail past ~9k
// that brings no improvement. 8,000 runes is generous (≈2–4k
// tokens depending on language density); skills past it are almost
// certainly bloated and should either be merged into an existing
// skill or trimmed. --force bypasses the cap for the rare
// legitimate case (a skill that genuinely needs a big body).
const skillMdBudgetRunes = 8000

// refuseOversizedSkill enforces skillMdBudgetRunes against a
// rendered SKILL.md body before it lands on disk. Returns a clear
// error pointing at the option (trim, merge, or --force) when the
// budget is exceeded; nil otherwise. Force=true is the operator
// escape hatch.
func refuseOversizedSkill(path, body string, force bool) error {
	runes := len([]rune(body))
	if runes <= skillMdBudgetRunes {
		return nil
	}
	if force {
		return nil
	}
	return fmt.Errorf("refusing %s: rendered SKILL.md is %d runes (budget %d). "+
		"Long skills cost tokens without helping (SWE-Skills-Bench: token "+
		"overhead is decoupled from pass-rate gain). Trim the proposal "+
		"(focus when_to_use, drop redundant triggers/examples), merge into "+
		"an existing skill via `aichronicles propose merge --skill %s`, or "+
		"pass --force to override",
		path, runes, skillMdBudgetRunes, filepath.Base(filepath.Dir(path)))
}

// skillProvenanceFingerprintLen is the number of leading hex
// characters from the SHA-256 we embed in the SKILL.md footer.
// 12 chars = 48 bits — plenty for human-readable cross-reference
// against the candidate row's add_body_sha256, while staying
// short enough to render compactly. The full hash lives in the
// DB, the fingerprint is just for visual identification.
const skillProvenanceFingerprintLen = 12

// skillProvenanceFooter is the trailing block aichronicles appends
// to a SKILL.md body after computing its hash. The line itself is
// not part of the hash (see addSkillCandidate's hash-then-append
// order), so a drift checker can strip the line, recompute the
// hash, and compare against skill_candidates.add_body_sha256.
//
// SSGM (Lam et al., 2026 — arXiv:2603.11768) calls this primitive
// "consistency verification": the lifecycle index has to be able
// to tell "what aichronicles wrote" from "what was edited
// afterwards." Without the hash, an undetected hand-edit silently
// invalidates every downstream signal that assumes the body still
// matches the LLM-emitted candidate.
func skillProvenanceFooter(bodySHA256 string) string {
	short := bodySHA256
	if len(short) > skillProvenanceFingerprintLen {
		short = short[:skillProvenanceFingerprintLen]
	}
	return fmt.Sprintf(
		"\n<!-- aichronicles-provenance: sha256:%s — drift check via "+
			"`aichronicles skills verify` (see skill_candidates.add_body_sha256). -->\n",
		short,
	)
}

// findProposedSkill matches by name, accepting either the kebab-
// case canonical name or a case-insensitive prefix when unique.
func findProposedSkill(r *prompts.ProposalResult, name string) (*prompts.ProposedSkill, error) {
	for i := range r.Skills {
		if r.Skills[i].Name == name {
			return &r.Skills[i], nil
		}
	}
	// Prefix-match fallback for typo / partial input.
	var hits []int
	low := strings.ToLower(name)
	for i, sk := range r.Skills {
		if strings.HasPrefix(strings.ToLower(sk.Name), low) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 1 {
		return &r.Skills[hits[0]], nil
	}
	if len(hits) > 1 {
		var names []string
		for _, h := range hits {
			names = append(names, r.Skills[h].Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("--skill %q is ambiguous (matches: %s)", name, strings.Join(names, ", "))
	}
	return nil, fmt.Errorf("--skill %q not found in proposal (run `aichronicles propose list` to see options)", name)
}

// skillFrontmatter is what we yaml.Marshal at the top of SKILL.md.
// Field names + omitempty match Claude Code's documented schema at
// https://code.claude.com/docs/en/skills#frontmatter-reference, with
// AutoSkill (Yang et al., 2026 — arXiv:2603.01145) skill-tuple
// metadata added as additional keys (YAML readers ignore unknown
// keys by spec; Claude Code's parser inherits that behaviour).
//
//   - name:         lowercase letters / numbers / hyphens only,
//     ≤64 chars. Maps directly to the proposal's
//     kebab-case name field; no transformation.
//   - description:  what the skill does and when. Front-loaded.
//   - when_to_use:  optional trigger phrases / example requests,
//     appended to description in the listing.
//   - version:      AutoSkill v — semver-ish identifier; v0.1.0 on
//     fresh add, patch-bumped by every merge.
//   - tags:         AutoSkill γ — categorical labels for browsing.
//   - triggers:     AutoSkill τ — short query-shaped phrases that
//     activate retrieval. Distinct from when_to_use:
//     when_to_use is descriptive prose, triggers are
//     the lookup terms.
//   - examples:     AutoSkill ξ — (input → output) demonstrations
//     of the skill's intended use, parsed from the
//     LLM's proposal verbatim.
//
// Combined description + when_to_use are capped at 1536 chars in
// the skill listing per the docs; we trim aggressively below that
// to stay safe.
type skillFrontmatter struct {
	Name        string                    `yaml:"name"`
	Description string                    `yaml:"description"`
	WhenToUse   string                    `yaml:"when_to_use,omitempty"`
	Version     string                    `yaml:"version,omitempty"`
	Tags        []string                  `yaml:"tags,omitempty"`
	Triggers    []string                  `yaml:"triggers,omitempty"`
	Examples    []skillFrontmatterExample `yaml:"examples,omitempty"`
}

// skillFrontmatterExample is one (input, output) demonstration in
// the AutoSkill ξ set, rendered as a YAML object inside the
// frontmatter array.
type skillFrontmatterExample struct {
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
}

// skillFrontmatterCharCap is the 1536-char ceiling Claude Code
// documents for the combined description + when_to_use text. We
// cap each field at half of that minus a small reserve so the
// combined block stays comfortably under the limit even when
// both fields are set.
const skillFrontmatterCharCap = 700

// renderSkillScaffold builds the SKILL.md body. Frontmatter is
// generated by yaml.v3 so quoting / escaping / line breaks are
// handled correctly without hand-rolled logic. The body
// deliberately mirrors the canonical examples in
// https://code.claude.com/docs/en/skills: a short intro paragraph
// and a single numbered Steps list. We do NOT invent
// section headers ("Pitfalls", "Verification") that aren't part
// of the documented format — Claude Code skills are intentionally
// minimal and the official examples don't carry them.
//
// Helper scripts are referenced inline as part of the Steps
// guidance (and in a trailing provenance footer) rather than in
// their own H2 section, matching the docs' "Reference supporting
// files from your SKILL.md so Claude knows what they contain"
// convention.
func renderSkillScaffold(sk *prompts.ProposedSkill, outputID int64) string {
	examples := make([]skillFrontmatterExample, 0, len(sk.Examples))
	for _, e := range sk.Examples {
		examples = append(examples, skillFrontmatterExample{
			Input:  e.Input,
			Output: e.Output,
		})
	}
	frontmatter, err := yaml.Marshal(skillFrontmatter{
		Name:        sk.Name, // kebab-case verbatim from the proposal
		Description: clipToRunes(buildSkillDescription(sk), skillFrontmatterCharCap),
		WhenToUse:   clipToRunes(strings.TrimSpace(sk.WhenToUse), skillFrontmatterCharCap),
		Version:     store.InitialSkillVersion, // fresh add — merge bumps
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
			sc.Name, lowerFirst(strings.TrimRight(strings.TrimSpace(sc.Purpose), ".")))
	}
	fmt.Fprintln(&b)

	// Provenance footer — kept compact. The skill body is
	// loaded into context every time the skill runs (per the
	// docs' content-lifecycle section), so a 50-row footer
	// would burn tokens for every invocation.
	fmt.Fprintln(&b, "---")
	fmt.Fprintf(&b, "*Scaffolded by `aichronicles propose add` from llm_outputs id=%d.*  \n", outputID)
	if sk.AlternativesRejected != "" {
		fmt.Fprintf(&b, "*Alternatives considered:* %s  \n", oneLine(sk.AlternativesRejected))
	}
	if len(sk.Evidence) > 0 {
		fmt.Fprintln(&b, "*Evidence sessions:*")
		for _, ev := range sk.Evidence {
			fmt.Fprintf(&b, "- `%s` — %s\n", ev.SessionID, oneLine(ev.WhatHappened))
		}
	}
	return b.String()
}

// buildSkillDescription assembles the `description` frontmatter
// field. Per the docs, this is what Claude reads to decide when
// to load the skill — front-load the key use case, keep it
// concrete. We splice when_to_use's trigger into the same
// sentence so a single line covers "what" + "when" before
// truncation.
func buildSkillDescription(sk *prompts.ProposedSkill) string {
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

// clipToRunes is a rune-safe truncation that lands cleanly on a
// word boundary when possible. Used for frontmatter scalars where
// the docs cap combined description + when_to_use at 1536 chars.
func clipToRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndexAny(cut, " ,;:"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

// lowerFirst lowercases the first rune of s, leaving the rest
// untouched. Used to splice script purposes into "Run scripts/foo
// to <purpose>" so the resulting sentence reads naturally.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToLower(string(r[0])))[0]
	return string(r)
}

// renderSkillScriptScaffold returns the body for one helper
// script under <skill>/scripts/<name>. Three populating paths,
// checked in priority order:
//
//  1. Steps[] (AWM-style parameterised template) — render each
//     step as a commented bash line, with a leading
//     "Placeholders:" doc-block listing each {token} the steps
//     reference along with its description and an example value
//     drawn from the cited evidence sessions.
//  2. Body (free-form starter the LLM grounded from evidence).
//     Dropped in verbatim under the header.
//  3. Neither — emit a TODO stub directing the user to fill in
//     by walking the cited sessions.
//
// The header always cites the originating skill and the
// proposal's llm_outputs id so provenance is greppable.
func renderSkillScriptScaffold(sc *prompts.ProposedSkillScript, sk *prompts.ProposedSkill, outputID int64) string {
	var b strings.Builder
	fmt.Fprintln(&b, "#!/usr/bin/env bash")
	fmt.Fprintln(&b, "# "+strings.TrimSpace(sc.Purpose))
	fmt.Fprintln(&b, "#")
	fmt.Fprintf(&b, "# Skill: %s\n", sk.Name)
	fmt.Fprintf(&b, "# Scaffolded by `aichronicles propose add` from llm_outputs id=%d.\n", outputID)

	switch {
	case len(sc.Steps) > 0:
		writePlaceholderBlock(&b, sc.Placeholders)
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

// writePlaceholderBlock renders a leading comment block that
// documents each {token} the script's steps reference. Skipped
// entirely when no placeholders are present so a fully-concrete
// script doesn't get a confusing empty block.
func writePlaceholderBlock(b *strings.Builder, placeholders []prompts.ProposedScriptPlaceholder) {
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

// resolveSkillsDir centralises the user-home default so tests can
// override it via flag without each apply* function carrying its
// own fallback.
func resolveSkillsDir(override string) string {
	if override != "" {
		return override
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude", "skills")
	}
	return ""
}

func refuseExistingUnlessForce(path string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists (pass --force to overwrite)", path)
	}
	return nil
}

// refuseDuplicateSkillName fires before the verify gate and the
// write path: if a skill with the candidate's kebab-case name
// already lives at the target SKILL.md path or in the global
// ~/.claude/skills directory, refuse and point the user at
// `propose merge` instead of `propose add`. Cheap: a couple of
// os.Stat calls; no LLM, no DB.
//
// Empirical motivation: the Claude skills marketplace data-driven
// study (Ling et al., 2026 — arXiv:2602.08004) reports 46.3% of
// listings are name-duplicates and AutoRefine (Qiu et al., 2026 —
// arXiv:2601.22758) shows that without active dedup a skill repo
// grows 4.5× and utilisation drops 8.9×. Aichronicles' propose
// step already shows installed skills to the LLM, but the LLM
// occasionally re-proposes the same name anyway; this is the
// hard guard at the maintenance-decision boundary.
//
// --force bypasses (the legitimate "I really do want to clobber"
// case). Returns nil when nothing collides; an error suggesting
// the merge / discard alternatives otherwise.
func refuseDuplicateSkillName(targetSkillMd, candidateName string, force bool) error {
	if force {
		return nil
	}

	// Primary: the target path (current behaviour, but with a
	// merge-pointing message).
	if _, err := os.Stat(targetSkillMd); err == nil {
		return fmt.Errorf("skill %q already exists at %s — "+
			"use `aichronicles propose merge --skill %s` to fold the "+
			"candidate into the existing skill, "+
			"`aichronicles propose discard --skill %s` if you do not "+
			"want it, or pass --force to overwrite",
			candidateName, targetSkillMd, candidateName, candidateName)
	}

	// Secondary: the global skills dir, when the user is adding to
	// a different (e.g. project-local) location. Catches the case
	// where the global already covers the trigger and the user is
	// about to add a project-local duplicate.
	home, herr := os.UserHomeDir()
	if herr != nil {
		return nil
	}
	globalPath := filepath.Join(home, ".claude", "skills", candidateName, "SKILL.md")
	if globalPath == targetSkillMd {
		return nil // already checked above
	}
	if _, err := os.Stat(globalPath); err == nil {
		return fmt.Errorf("skill %q already exists globally at %s — "+
			"add it project-local would create a duplicate; "+
			"either run `aichronicles propose merge --skill %s` against "+
			"the global skill, `aichronicles propose discard --skill %s` "+
			"if it is no longer relevant, or pass --force to add anyway",
			candidateName, globalPath, candidateName, candidateName)
	}
	return nil
}

// oneLine collapses any whitespace run (newlines included) into
// single spaces so a multi-line proposal field renders cleanly
// inside a list item or comment.
func oneLine(s string) string {
	return collapseWhitespace(strings.TrimSpace(s))
}

// oneLineN is oneLine + a rune-count cap. Used for YAML scalars
// (which we keep short to stay readable) and for list previews
// in `propose list`.
func oneLineN(s string, n int) string {
	out := oneLine(s)
	r := []rune(out)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return out
}
