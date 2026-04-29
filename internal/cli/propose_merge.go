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
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
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

			if skillName == "" {
				return errors.New("--skill <name> is required (run `aichronicles propose list` to see options)")
			}

			result, output, err := loadLatestProposal(cmd.Context(), s, outputID)
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

			return mergeProposedSkill(ctx, s, result, output.ID, skillName,
				resolveSkillsDir(skillsDir), noVerify,
				newClient, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
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
	r *prompts.ProposalResult,
	outputID int64,
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

	if !noVerify {
		if err := verifyProposalOrAbort(ctx, st, candidate, outputID, newClient, out); err != nil {
			return err
		}
	}

	merged, err := runMergeLLM(ctx, st, *candidate, string(existingBytes), nextVersion, outputID, newClient, out)
	if err != nil {
		return err
	}
	if err := writeMergedSkill(skillMd, merged, nextVersion); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "merged %s (%s → %s)\n", skillMd, existingVersion, nextVersion)
	if merged.Rationale != "" {
		_, _ = fmt.Fprintf(out, "  rationale: %s\n", merged.Rationale)
	}

	now := time.Now().UnixMilli()
	existingCandidate, err := store.LoadAddedSkillCandidate(ctx, st.DB(), candidate.Name)
	if err != nil {
		slog.Warn("merge: failed to load existing candidate row (lifecycle not recorded)", "err", err)
		return nil
	}
	if existingCandidate == nil {
		// The on-disk SKILL.md is hand-authored — there is no
		// candidate row to point merged_into_id at. Lifecycle
		// tracking still records the merge: we mark this candidate
		// as 'merge' with merged_into_id NULL via a discard-shaped
		// path? No — that conflates with discard. Instead, leave
		// the candidate as 'pending' so it surfaces as
		// "the user merged it into a hand-authored skill"; this is
		// rare enough that the simpler representation is fine.
		_, _ = fmt.Fprintln(out, "  (existing skill is hand-authored — no candidate row to record merge target)")
		return nil
	}
	if merr := store.MarkSkillCandidateMerged(ctx, st.DB(), outputID, candidate.Name,
		existingCandidate.ID, skillMd, now); merr != nil {
		if errors.Is(merr, store.ErrSkillCandidateNotFound) {
			if rerr := store.RecordSkillCandidate(ctx, st.DB(), outputID, candidate.Name, now); rerr == nil {
				_ = store.MarkSkillCandidateMerged(ctx, st.DB(), outputID, candidate.Name,
					existingCandidate.ID, skillMd, now)
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
	st *store.Store,
	candidate prompts.ProposedSkill,
	currentSkillMd, nextVersion string,
	outputID int64,
	newClient func() (llm.Client, error),
	out io.Writer,
) (*prompts.MergedSkillResult, error) {
	hash := proposeMergeHash(outputID, candidate.Name, currentSkillMd, nextVersion)

	cached, err := store.LoadLLMOutputByHash(ctx, st.DB(), store.LLMKindSkillMerge, hash)
	if err != nil {
		return nil, fmt.Errorf("merge: cache lookup: %w", err)
	}
	if cached != nil {
		var m prompts.MergedSkillResult
		if jerr := json.Unmarshal([]byte(cached.Body), &m); jerr == nil {
			_, _ = fmt.Fprintln(out, "merge: ✓ result cached, no LLM call")
			return &m, nil
		}
		slog.Warn("merge: malformed cached body, re-running", "id", cached.ID, "err", err)
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
	if _, err := persistSummary(ctx, st, &persistInput{
		kind:       store.LLMKindSkillMerge,
		hash:       hash,
		model:      resp.Model,
		inputToks:  resp.Usage.InputTokens,
		outputToks: resp.Usage.OutputTokens,
		body:       body,
	}); err != nil {
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

// writeMergedSkill rebuilds the SKILL.md from the merged result:
// regenerate the YAML frontmatter from the merged scalars + the
// supplied next_version, then concatenate the body markdown the
// LLM emitted (without the frontmatter fences — see the merge
// system prompt). Atomic via a tmp-file + rename so a crash
// mid-write can't corrupt the existing skill.
func writeMergedSkill(path string, merged *prompts.MergedSkillResult, nextVersion string) error {
	examples := make([]skillFrontmatterExample, 0, len(merged.Examples))
	for _, e := range merged.Examples {
		examples = append(examples, skillFrontmatterExample{
			Input:  e.Input,
			Output: e.Output,
		})
	}
	frontmatter, err := yaml.Marshal(skillFrontmatter{
		Name:        merged.Name,
		Description: merged.Description,
		WhenToUse:   merged.WhenToUse,
		Version:     nextVersion,
		Tags:        append([]string(nil), merged.Tags...),
		Triggers:    append([]string(nil), merged.Triggers...),
		Examples:    examples,
	})
	if err != nil {
		return fmt.Errorf("marshal frontmatter: %w", err)
	}

	var buf []byte
	buf = append(buf, "---\n"...)
	buf = append(buf, frontmatter...)
	buf = append(buf, "---\n\n"...)
	buf = append(buf, merged.BodyMarkdown...)
	if len(merged.BodyMarkdown) == 0 || merged.BodyMarkdown[len(merged.BodyMarkdown)-1] != '\n' {
		buf = append(buf, '\n')
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
