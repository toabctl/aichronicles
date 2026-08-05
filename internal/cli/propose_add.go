package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/skillscaffold"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/textfmt"
	"github.com/toabctl/aichronicles/internal/timefmt"
	"github.com/toabctl/aichronicles/internal/wire"
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
		sockFlag  string
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
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}

			if skillName == "" {
				return errors.New("--skill <name> is required (run `aichronicles propose list` to see options)")
			}

			result, output, err := loadLatestProposal(cmd.Context(), c, outputID)
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

			return addSkillCandidate(ctx, s, c, result, output.ID, output.CreatedAtMs, skillName,
				resolveSkillsDir(skillsDir), force, noVerify,
				newClient, cmd.OutOrStdout())
		},
	}
	addDBFlag(cmd, &dbPath)
	addSocketFlag(cmd, &sockFlag)
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
		sockFlag string
		outputID int64
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List skills in the latest cached propose output",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			result, output, err := loadLatestProposal(cmd.Context(), c, outputID)
			if err != nil {
				return err
			}
			renderProposalIndex(cmd.OutOrStdout(), result, output)
			return nil
		},
	}
	addSocketFlag(cmd, &sockFlag)
	cmd.Flags().Int64Var(&outputID, "output-id", 0,
		"specific llm_outputs row id (default: latest propose row)")
	return cmd
}

// loadLatestProposal returns the parsed ProposalResult plus the
// underlying llm_outputs row. When wantID > 0 the matching row is
// loaded directly; otherwise we return the most recent
// kind=propose row. Both paths produce a clear error when no
// proposal has been generated yet.
func loadLatestProposal(ctx context.Context, c *apiclient.Client, wantID int64) (*prompts.ProposalResult, *wire.LLMOutput, error) {
	var output *wire.LLMOutput
	if wantID > 0 {
		out, err := c.LLMOutputByID(ctx, wantID)
		if err != nil {
			return nil, nil, fmt.Errorf("load propose output id=%d: %w", wantID, err)
		}
		if out.Kind != string(store.LLMKindPropose) && out.Kind != string(store.LLMKindInduction) {
			return nil, nil, fmt.Errorf(
				"llm_outputs id=%d has kind %q; expected %q or %q",
				wantID, out.Kind, store.LLMKindPropose, store.LLMKindInduction)
		}
		output = &out
	} else {
		rows, err := c.LLMOutputsList(ctx, string(store.LLMKindPropose), "", 1)
		if err != nil {
			return nil, nil, fmt.Errorf("load latest propose: %w", err)
		}
		if len(rows) == 0 {
			return nil, nil, errors.New("no cached propose output found — run `aichronicles propose` first")
		}
		output = &rows[0]
	}

	result, err := proposalResultFromOutput(output)
	if err != nil {
		return nil, nil, err
	}
	// Anti-fabrication grounding: drop triggers the LLM emitted
	// without anchoring in evidence Quote text. Done once at the
	// load boundary so every consumer (propose add / merge / verify
	// rendering / persistence) sees the same filtered set. See
	// prompts.FilterGroundedTriggers for the rule.
	result.GroundTriggers()
	// Anti-fabrication grounding II: drop evidence rows whose
	// SessionID doesn't resolve to a real session via the api. The
	// schema's UUIDv5 pattern catches obviously-malformed IDs; this
	// catches the harder case — a syntactically valid but
	// hallucinated UUID. Without it the SKILL.md provenance footer
	// and `/propose` web hyperlinks confidently cite sessions that
	// don't exist (review 2026-05-14 P0).
	resolved := resolveEvidenceSessions(ctx, c, result)
	result.GroundEvidence(resolved)
	return result, output, nil
}

// proposalResultFromOutput decodes an llm_outputs body into the
// proposal shape `propose add` works with, accepting both the
// multi-skill propose body and the single-skill induction body.
//
// Induction is a first-class producer of skill candidates: it writes
// a skill_candidates row per induced skill and renderInductionResult
// prints "add: aichronicles propose add --skill X --output-id <id>".
// That command could never work, because the loader rejected any kind
// but propose — so induced candidates accumulated in `pending`
// forever, and renderPriorProposals then fed them back to the propose
// LLM as "user did not act on this suggestion", teaching the corpus
// to stop proposing them. The online-induction half of the AutoSkill
// lifecycle was effectively write-only.
//
// No translation is needed: InductionResult.Skill is a *ProposedSkill,
// the same type ProposalResult.Skills holds, and its doc comment
// already says propose add "consumes it without translation".
func proposalResultFromOutput(output *wire.LLMOutput) (*prompts.ProposalResult, error) {
	if output.Kind == string(store.LLMKindInduction) {
		var induced prompts.InductionResult
		if err := json.Unmarshal([]byte(output.Body), &induced); err != nil {
			return nil, fmt.Errorf("parse cached induction body (id=%d): %w", output.ID, err)
		}
		if induced.Skill == nil {
			return nil, fmt.Errorf(
				"induction output id=%d induced no skill (only a workflow, or nothing) — "+
					"there is nothing to add", output.ID)
		}
		return &prompts.ProposalResult{Skills: []prompts.ProposedSkill{*induced.Skill}}, nil
	}

	var result prompts.ProposalResult
	if err := json.Unmarshal([]byte(output.Body), &result); err != nil {
		return nil, fmt.Errorf("parse cached propose body (id=%d): %w", output.ID, err)
	}
	return &result, nil
}

// resolveEvidenceSessions returns the subset of distinct evidence
// SessionIDs across r.Skills that resolve to a real session via
// c.Session. Anything that fails to resolve (not found, transport
// error) is omitted — drop unresolvable references rather than
// emit citations the user can't follow. One round-trip per distinct
// id; propose payloads typically cite ≤25 sessions total so the
// cost is bounded by the proposal size, not the backlog.
func resolveEvidenceSessions(ctx context.Context, c *apiclient.Client, r *prompts.ProposalResult) map[string]struct{} {
	seen := map[string]struct{}{}
	for i := range r.Skills {
		for _, e := range r.Skills[i].Evidence {
			id := strings.TrimSpace(e.SessionID)
			if id == "" {
				continue
			}
			seen[id] = struct{}{}
		}
	}
	out := make(map[string]struct{}, len(seen))
	for id := range seen {
		if _, err := c.Session(ctx, id); err == nil {
			out[id] = struct{}{}
		}
	}
	return out
}

// renderProposalIndex prints a short tabular view of every skill in
// the proposal — its position, its name, evidence count, effort,
// and a one-line trigger preview. Output is plain text so a user
// can pipe it through grep / awk.
func renderProposalIndex(out io.Writer, r *prompts.ProposalResult, output *wire.LLMOutput) {
	_, _ = fmt.Fprintf(out, "propose output id=%d  generated=%s  model=%s\n\n",
		output.ID,
		timefmt.Absolute(output.CreatedAtMs),
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
		_, _ = fmt.Fprintf(out, "      %s\n", textfmt.OneLine(sk.WhenToUse))
		for _, sc := range sk.Scripts {
			_, _ = fmt.Fprintf(out, "      └── scripts/%s  — %s\n", sc.Name, textfmt.OneLineN(sc.Purpose, 80))
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
	c *apiclient.Client,
	r *prompts.ProposalResult,
	outputID int64,
	outputCreatedAtMs int64,
	name, root string,
	force, noVerify bool,
	newClient func() (llm.Client, error),
	out io.Writer,
) error {
	sk, err := findProposedSkill(r, name)
	if err != nil {
		return err
	}

	// Validate before any path is built. sk.Name and each script name
	// come straight out of the model's tool call, and filepath.Join
	// resolves ".." rather than rejecting it — so an unvalidated name
	// escapes the skills tree, and scripts land mode 0755.
	//
	// --skill also resolves by unique case-insensitive prefix, so the
	// user never has to see or type the full name they are approving.
	scriptNames := make([]string, 0, len(sk.Scripts))
	for _, sc := range sk.Scripts {
		scriptNames = append(scriptNames, sc.Name)
	}
	if err := skillscaffold.ValidateProposedSkillNames(sk.Name, scriptNames); err != nil {
		return fmt.Errorf("refusing to write proposal (id=%d): %w", outputID, err)
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

	// Discard-history check: if the user explicitly discarded a
	// candidate with this skill name in any prior session, refuse
	// the add unless --force. Without this, `propose discard
	// --skill X` is undermined — a fresh `propose add --skill X`
	// from a different output_id silently re-adds the rejected
	// idea, which is exactly the "rejection signal for future
	// propose runs" promise the discard command makes.
	if err := refuseDiscardedSkillName(ctx, c, sk.Name, force); err != nil {
		return err
	}

	if !noVerify {
		if err := verifyProposalOrAbort(ctx, st, c, sk, outputID, newClient, out); err != nil {
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
	// skillscaffold.Render is the single source of truth for the
	// SKILL.md format (shared with the /propose preview). It returns
	// the pre-footer body (what the size budget checks), its SHA-256
	// (recorded on the candidate row for drift detection), and the
	// full body-plus-footer bytes we write to disk.
	rendered := skillscaffold.Render(sk, outputID)
	if err := refuseOversizedSkill(skillMd, rendered.Body, force); err != nil {
		return err
	}
	bodyHashHex := rendered.SHA256
	if err := os.WriteFile(skillMd, []byte(rendered.Full), 0o644); err != nil {
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
			scriptBody := skillscaffold.RenderScript(&sc, sk, outputID)
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
	dec := wire.SkillCandidateDecisionRequest{
		LLMOutputID:  outputID,
		SkillName:    sk.Name,
		Decision:     wire.DecisionAdd,
		DecisionAtMs: now,
		AddPath:      skillMd,
		BodySHA256:   bodyHashHex,
	}
	if merr := c.SkillCandidateDecision(ctx, dec); merr != nil {
		if errors.Is(merr, apiclient.ErrNotFound) {
			anchor := outputCreatedAtMs
			if anchor <= 0 {
				anchor = now
			}
			if _, rerr := c.RecordSkillCandidate(ctx, wire.RecordSkillCandidateRequest{
				LLMOutputID:  outputID,
				SkillName:    sk.Name,
				ProposedAtMs: anchor,
			}); rerr == nil {
				_ = c.SkillCandidateDecision(ctx, dec)
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

// refuseExistingUnlessForce rejects writes that would clobber an
// existing entry at path, unless --force is passed. Uses os.Lstat
// rather than os.Stat so a *dangling* symlink at the path also
// counts as existing — otherwise a symlink whose target doesn't
// exist returns ErrNotExist from Stat, the check clears, and the
// subsequent os.WriteFile follows the link and writes at the
// target's path (potentially outside the skills directory). That
// is the documented foot-gun this guard exists to close.
//
// A residual TOCTOU window remains between this Lstat and the
// downstream os.WriteFile: if the path appears in between, --force
// is implicitly granted. The blast radius is bounded because the
// caller has already validated the kebab-case skill name and the
// parent directory is created with 0o755 just above the write.
func refuseExistingUnlessForce(path string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
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

// refuseDiscardedSkillName rejects an `add` against a skill name
// that the user explicitly discarded in any prior run, unless
// --force is set. Looks up skill_candidates rows with
// decision='discard' for this name; the most recent decision_at_ms
// makes the error message actionable ("you discarded this on Y").
//
// Returns nil for skill names that have never been discarded, or
// for the discard → re-add flow under --force.
func refuseDiscardedSkillName(ctx context.Context, c *apiclient.Client, candidateName string, force bool) error {
	if force {
		return nil
	}
	resp, err := c.SkillCandidatesByName(ctx, candidateName, 0)
	if err != nil {
		// Soft-fail: a transient api error here shouldn't block the
		// add path entirely. The on-disk dedup check already ran;
		// log and proceed.
		slog.Warn("propose add: failed to check discard history", "skill", candidateName, "err", err)
		return nil
	}
	for _, r := range resp.Candidates {
		if r.Decision == string(store.MaintenanceDiscard) {
			when := "earlier"
			if r.DecisionAtMs != nil {
				when = timefmt.Absolute(*r.DecisionAtMs)
			}
			return fmt.Errorf("skill %q was previously discarded (%s) — "+
				"add it again only if you've changed your mind. "+
				"Pass --force to override the discard signal",
				candidateName, when)
		}
	}
	return nil
}
