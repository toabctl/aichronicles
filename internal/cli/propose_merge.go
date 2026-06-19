package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/skillscaffold"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// newProposeMergeCmd is the AutoSkill maintenance action 'merge'
// command: combine the freshly-extracted candidate with an existing
// SKILL.md on disk, bump the patch version, write the merged
// SKILL.md in place, and record the lifecycle transition. The
// existing skill must have the same kebab-case name as the
// candidate (AutoSkill rule: merge preserves identity).
//
// Flags mirror `propose add`: --skill, --output-id, --skills-dir,
// --no-verify (the verify gate runs the same critic — a bad
// candidate is bad whether you add it fresh or merge it in).
func newProposeMergeCmd() *cobra.Command {
	var (
		dbPath    string
		sockFlag  string
		skillName string
		outputID  int64
		skillsDir string
		noVerify  bool
	)
	cmd := &cobra.Command{
		Use:   "merge --skill <name>",
		Short: "Merge a proposed skill into the existing on-disk SKILL.md (AutoSkill action 'merge')",
		Long: "Loads the latest cached `propose` output (or the one\n" +
			"identified by --output-id), finds <name> in it, and merges\n" +
			"that candidate into the existing ~/.claude/skills/<name>/\n" +
			"SKILL.md per the AutoSkill (Yang et al., 2026 —\n" +
			"arXiv:2603.01145) maintenance rules: preserve the original\n" +
			"capability identity, semantic union not raw concatenation,\n" +
			"no regressions.\n\n" +
			"Bumps the patch component of the existing SKILL.md's\n" +
			"version field (v0.1.0 → v0.1.1). Records the lifecycle\n" +
			"transition on the candidate's skill_candidates row\n" +
			"(decision='merge', merged_into_id=existing-candidate-id).\n\n" +
			"Verification gate runs by default — same critic that\n" +
			"`propose add` invokes. Pass --no-verify to bypass.",
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

			return mergeProposedSkill(ctx, s, c, result, output.ID, output.CreatedAtMs, skillName,
				resolveSkillsDir(skillsDir), noVerify,
				newClient, cmd.OutOrStdout())
		},
	}
	addDBFlag(cmd, &dbPath)
	addSocketFlag(cmd, &sockFlag)
	cmd.Flags().StringVar(&skillName, "skill", "",
		"name of a skill from the proposal to merge into its on-disk twin")
	cmd.Flags().Int64Var(&outputID, "output-id", 0,
		"specific llm_outputs row id (default: latest propose row)")
	cmd.Flags().StringVar(&skillsDir, "skills-dir", "",
		"override target directory (default: ~/.claude/skills)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false,
		"skip the critic-LLM verification gate")
	return cmd
}

// mergeProposedSkill is the merge workflow: read existing SKILL.md,
// bump version, call BuildMergeSkill, write the merged SKILL.md,
// and record the lifecycle transition. Pure orchestration — every
// step's primitive is in store / skills / prompts.
//
// Merges always preserve the kebab-case name (AutoSkill rule); the
// existing skill must live at <root>/<name>/SKILL.md. A missing
// existing skill is an error — the caller meant to run `propose
// apply` (add) instead.
func mergeProposedSkill(
	ctx context.Context,
	st *store.Store,
	c *apiclient.Client,
	r *prompts.ProposalResult,
	outputID int64,
	outputCreatedAtMs int64,
	name, root string,
	noVerify bool,
	newClient func() (llm.Client, error),
	out io.Writer,
) error {
	candidate, err := findProposedSkill(r, name)
	if err != nil {
		return err
	}

	skillMd := filepath.Join(root, candidate.Name, "SKILL.md")
	existingBytes, err := os.ReadFile(skillMd)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("merge: %s does not exist; use `aichronicles propose add --skill %s` for a fresh add",
				skillMd, candidate.Name)
		}
		return fmt.Errorf("read existing SKILL.md: %w", err)
	}

	existingVersion, _ := skills.ReadVersion(skillMd)
	if existingVersion == "" {
		// Pre-AutoSkill skills have no version key. Treat them as
		// at the initial version so the patch-bump produces a
		// sensible v0.1.1.
		existingVersion = store.InitialSkillVersion
	}
	nextVersion := store.BumpPatch(existingVersion)

	// Resolve the merge target up front so we can reject self-merge
	// BEFORE spending an LLM round-trip. LoadAddedSkillCandidate
	// filters by skill_name only — its return CAN be the same row
	// owned by this very `outputID` if the user already ran
	// `propose add` against this output and is now running merge
	// against it. The unconditional UPDATE in MarkSkillCandidateMerged
	// would otherwise satisfy `merged_into_id = id` (the FK doesn't
	// reject self-references), producing a row pointing at itself.
	// Loading once here also keeps a single canonical view of the
	// target row through the whole flow.
	var existingCandidate *wire.SkillCandidate
	{
		row, lerr := c.AddedSkillCandidate(ctx, candidate.Name)
		switch {
		case lerr == nil:
			existingCandidate = &row
		case errors.Is(lerr, apiclient.ErrNotFound):
			// Hand-authored skill — no candidate row owns the file.
			existingCandidate = nil
		default:
			return fmt.Errorf("merge: load existing candidate: %w", lerr)
		}
	}
	if existingCandidate != nil &&
		existingCandidate.LLMOutputID == outputID &&
		existingCandidate.SkillName == candidate.Name {
		return fmt.Errorf("merge: candidate (output=%d, skill=%q) is already added at %s; merge target must be a different candidate. Run `propose discard` first if you meant to undo, or pick a different --output-id",
			outputID, candidate.Name, skillMd)
	}

	if !noVerify {
		if err := verifyProposalOrAbort(ctx, st, c, candidate, outputID, newClient, out); err != nil {
			return err
		}
	}

	merged, err := runMergeLLM(ctx, c, *candidate, string(existingBytes), nextVersion, outputID, newClient, out)
	if err != nil {
		return err
	}
	bodyHashHex, err := writeMergedSkill(skillMd, merged, nextVersion)
	if err != nil {
		return err
	}
	// Persist any scripts the merge result claims as the deduped
	// union. Pre-fix, candidate-side scripts were silently dropped
	// at merge time even when the LLM recognised them as additive;
	// the merge schema now carries `scripts` so the LLM emits the
	// union and the CLI writes it. We mirror propose_add's loop:
	// scripts/<name> with 0o755 (executable), header carrying the
	// originating outputID for grep-able provenance.
	if len(merged.Scripts) > 0 {
		scriptsDir := filepath.Join(filepath.Dir(skillMd), "scripts")
		if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
			return fmt.Errorf("ensure %s: %w", scriptsDir, err)
		}
		for _, sc := range merged.Scripts {
			target := filepath.Join(scriptsDir, sc.Name)
			scriptBody := renderMergedScriptScaffold(&sc, candidate.Name, outputID)
			if werr := os.WriteFile(target, []byte(scriptBody), 0o755); werr != nil {
				return fmt.Errorf("write %s: %w", target, werr)
			}
			_, _ = fmt.Fprintf(out, "wrote %s (executable)\n", target)
		}
	}
	// Refresh the surviving (target) candidate's add_body_sha256 to
	// match the just-written merged file. Without this, the DB
	// hash points at the pre-merge content while the on-disk file
	// is the post-merge content, and the next `skills verify`
	// reports every merged skill as drifted.
	if existingCandidate != nil {
		// Combine the body-hash + kind refresh into a single PATCH.
		// The api endpoint applies whichever fields are set; an
		// out-of-enum kind leaves the row's existing kind untouched
		// (no fabrication — CLAUDE.md rule #7).
		updateReq := wire.UpdateSkillCandidateRequest{
			AddPath:    skillMd,
			BodySHA256: bodyHashHex,
		}
		if mergedKind := store.SkillKind(merged.Kind); mergedKind == store.SkillKindPattern || mergedKind == store.SkillKindPitfall {
			updateReq.Kind = string(mergedKind)
		}
		if uerr := c.UpdateSkillCandidate(ctx, existingCandidate.ID, updateReq); uerr != nil {
			slog.Warn("merge: failed to refresh merge target",
				"id", existingCandidate.ID, "err", uerr)
		}
	}
	_, _ = fmt.Fprintf(out, "merged %s (%s → %s)\n", skillMd, existingVersion, nextVersion)
	if merged.Rationale != "" {
		_, _ = fmt.Fprintf(out, "  rationale: %s\n", merged.Rationale)
	}

	now := time.Now().UnixMilli()

	// Pick the merge target id: a real candidate row (existing add)
	// or 0 (sentinel for "hand-authored skill — no candidate row to
	// FK to"). Recording the merge in either case is what closes
	// the rejection-signal feedback loop: a candidate left in
	// `pending` is read by future propose runs as "user ignored
	// the suggestion", which is the wrong inference when the user
	// actually folded it into a hand-authored skill.
	var mergedIntoID int64
	if existingCandidate != nil {
		mergedIntoID = existingCandidate.ID
	} else {
		_, _ = fmt.Fprintln(out, "  (existing skill is hand-authored — recording merge with NULL target)")
	}
	dec := wire.SkillCandidateDecisionRequest{
		LLMOutputID:  outputID,
		SkillName:    candidate.Name,
		Decision:     wire.DecisionMerge,
		DecisionAtMs: now,
		AddPath:      skillMd,
		MergedIntoID: mergedIntoID,
	}
	if merr := c.SkillCandidateDecision(ctx, dec); merr != nil {
		if errors.Is(merr, apiclient.ErrNotFound) {
			anchor := outputCreatedAtMs
			if anchor <= 0 {
				anchor = now
			}
			if _, rerr := c.RecordSkillCandidate(ctx, wire.RecordSkillCandidateRequest{
				LLMOutputID:  outputID,
				SkillName:    candidate.Name,
				ProposedAtMs: anchor,
			}); rerr == nil {
				_ = c.SkillCandidateDecision(ctx, dec)
			}
		} else {
			slog.Warn("merge: failed to record skill_candidates merge", "err", merr)
		}
	}
	return nil
}

// runMergeLLM threads a BuildMergeSkill call through runCachedLLM,
// caching the result under kind=skill_merge keyed on
// (outputID, skill-name). A re-run on the same proposal is free.
func runMergeLLM(
	ctx context.Context,
	c *apiclient.Client,
	candidate prompts.ProposedSkill,
	currentSkillMd, nextVersion string,
	outputID int64,
	newClient func() (llm.Client, error),
	out io.Writer,
) (*prompts.MergedSkillResult, error) {
	hash := proposeMergeHash(outputID, candidate.Name, currentSkillMd, nextVersion)

	cached, err := c.LLMOutputByHash(ctx, string(store.LLMKindSkillMerge), hash)
	switch {
	case err == nil:
		var m prompts.MergedSkillResult
		jerr := json.Unmarshal([]byte(cached.Body), &m)
		if jerr == nil {
			_, _ = fmt.Fprintln(out, "merge: ✓ result cached, no LLM call")
			return &m, nil
		}
		slog.Warn("merge: malformed cached body, re-running", "id", cached.ID, "err", jerr)
	case errors.Is(err, apiclient.ErrNotFound):
		// fall through to the LLM call
	default:
		return nil, fmt.Errorf("merge: cache lookup: %w", err)
	}

	built, err := prompts.BuildMergeSkill(prompts.MergeSkillInputs{
		SkillName:      candidate.Name,
		CurrentSkillMd: currentSkillMd,
		Candidate:      candidate,
		NextVersion:    nextVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("merge: build prompt: %w", err)
	}

	client, err := newClient()
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}
	resp, err := client.Complete(ctx, built.Request)
	if err != nil {
		return nil, fmt.Errorf("merge: LLM call: %w", err)
	}

	var merged prompts.MergedSkillResult
	if err := parseToolResult(resp, prompts.ToolNameSkillMerge, &merged); err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}

	body, err := marshalLLMBody(&merged)
	if err != nil {
		return nil, fmt.Errorf("merge: marshal: %w", err)
	}
	saveReq := wire.SaveLLMOutputRequest{
		Kind:        string(store.LLMKindSkillMerge),
		Model:       resp.Model,
		PromptHash:  hash,
		Body:        body,
		CreatedAtMs: time.Now().UnixMilli(),
	}
	if resp.Usage.InputTokens > 0 {
		v := int64(resp.Usage.InputTokens)
		saveReq.InputTokens = &v
	}
	if resp.Usage.OutputTokens > 0 {
		v := int64(resp.Usage.OutputTokens)
		saveReq.OutputTokens = &v
	}
	if _, err := c.SaveLLMOutput(ctx, saveReq); err != nil {
		slog.Warn("merge: persist failed (merge still applied)", "err", err)
	}
	return &merged, nil
}

// proposeMergeHash is the cache key for one merge call: a
// deterministic hash over (outputID, skill-name, current-skill-md,
// next-version). Including the existing SKILL.md bytes means a
// hand-edit to the on-disk file invalidates the cache automatically
// (same pattern as evolve's hash).
func proposeMergeHash(outputID int64, skillName, currentSkillMd, nextVersion string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(outputID, 10)))
	h.Write([]byte{'\x00'})
	h.Write([]byte(skillName))
	h.Write([]byte{'\x00'})
	h.Write([]byte(currentSkillMd))
	h.Write([]byte{'\x00'})
	h.Write([]byte(nextVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// renderMergedScriptScaffold mirrors renderSkillScriptScaffold for
// the merge path. Same body shape (steps / placeholders / fallback
// TODO) but the header notes "merged" provenance so a future
// reader can tell merge-written scripts from add-written ones.
func renderMergedScriptScaffold(sc *prompts.ProposedSkillScript, skillName string, outputID int64) string {
	var b strings.Builder
	fmt.Fprintln(&b, "#!/usr/bin/env bash")
	fmt.Fprintln(&b, "# "+strings.TrimSpace(sc.Purpose))
	fmt.Fprintln(&b, "#")
	fmt.Fprintf(&b, "# Skill: %s\n", skillName)
	fmt.Fprintf(&b, "# Scaffolded by `aichronicles propose merge` from llm_outputs id=%d.\n", outputID)

	switch {
	case len(sc.Steps) > 0:
		skillscaffold.WritePlaceholderBlock(&b, sc.Placeholders)
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
		fmt.Fprintln(&b, "# the merged source skills relied on.")
		fmt.Fprintln(&b, "echo 'TODO: implement' >&2")
		fmt.Fprintln(&b, "exit 1")
	}
	return b.String()
}

// writeMergedSkill rebuilds the SKILL.md from the merged result:
// regenerate the YAML frontmatter from the merged scalars + the
// supplied next_version, then concatenate the body markdown the
// LLM emitted (without the frontmatter fences — see the merge
// system prompt), append the SSGM provenance footer carrying the
// new body hash, and atomically rename. Returns the body hash so
// the caller can UPDATE the surviving candidate's add_body_sha256
// — without that, the on-disk hash drifts from the DB record and
// the next `skills verify` flags every merged skill as tampered.
func writeMergedSkill(path string, merged *prompts.MergedSkillResult, nextVersion string) (bodyHashHex string, err error) {
	examples := make([]skillscaffold.FrontmatterExample, 0, len(merged.Examples))
	for _, e := range merged.Examples {
		examples = append(examples, skillscaffold.FrontmatterExample{
			Input:  e.Input,
			Output: e.Output,
		})
	}
	frontmatter, err := yaml.Marshal(skillscaffold.Frontmatter{
		Name:        merged.Name,
		Description: merged.Description,
		WhenToUse:   merged.WhenToUse,
		Version:     nextVersion,
		Kind:        skillscaffold.FrontmatterKind(merged.Kind),
		Tags:        append([]string(nil), merged.Tags...),
		Triggers:    append([]string(nil), merged.Triggers...),
		Examples:    examples,
	})
	if err != nil {
		return "", fmt.Errorf("marshal frontmatter: %w", err)
	}

	var buf []byte
	buf = append(buf, "---\n"...)
	buf = append(buf, frontmatter...)
	buf = append(buf, "---\n\n"...)
	buf = append(buf, merged.BodyMarkdown...)
	if len(merged.BodyMarkdown) == 0 || merged.BodyMarkdown[len(merged.BodyMarkdown)-1] != '\n' {
		buf = append(buf, '\n')
	}

	// Hash the body BEFORE appending the provenance footer (the
	// footer encodes this hash, so the relationship is
	// "compute → append → write"; a drift checker reverses it as
	// "read → strip footer → recompute → compare"). Same shape
	// propose_add uses (skillProvenanceFooter), kept symmetric so
	// `skills verify` doesn't need to special-case merge output.
	sum := sha256.Sum256(buf)
	bodyHashHex = hex.EncodeToString(sum[:])
	buf = append(buf, skillscaffold.ProvenanceFooter(bodyHashHex)...)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename: %w", err)
	}
	return bodyHashHex, nil
}
