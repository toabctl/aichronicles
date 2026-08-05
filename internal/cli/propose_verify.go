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
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
)

// newProposeVerifyCmd surfaces the Voyager-style critic gate as its
// own leaf command. The same critic that `propose add` runs (via
// verifyProposalOrAbort) can be invoked standalone for a dry run:
// load the latest proposal, pick a skill, see the critic's verdict
// without writing any files. Exit code mirrors the gate — non-zero
// on refusal so the command composes with shell pipelines.
//
// Reuses the propose_verify cache (kind=propose_verify keyed by
// outputID + skill name), so calling `propose verify` then `propose
// add` is free for the second call.
func newProposeVerifyCmd() *cobra.Command {
	var (
		sockFlag  string
		skillName string
		outputID  int64
	)
	cmd := &cobra.Command{
		Use:   "verify --skill <name>",
		Short: "Run the propose-add critic gate without writing anything",
		Long: "Loads the latest cached propose output (or the one identified\n" +
			"by --output-id), finds the named skill, and runs the Voyager-\n" +
			"style critic LLM against it — the same check `propose add`\n" +
			"runs unless --no-verify is set.\n\n" +
			"Useful for: previewing the critic's verdict before committing\n" +
			"to an add; debugging a refusal you saw in `propose add`;\n" +
			"warming the kind=propose_verify cache so a subsequent add\n" +
			"is free.\n\n" +
			"Returns exit code 0 when the critic approves, non-zero with\n" +
			"the concern + recommendation on refusal. Requires " + llm.APIKeyEnv + "\n" +
			"on a cache miss.",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			sk, err := findProposedSkill(result, skillName)
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
			return verifyProposalOrAbort(ctx, c, sk, output.ID, newClient, cmd.OutOrStdout())
		},
	}
	addSocketFlag(cmd, &sockFlag)
	cmd.Flags().StringVar(&skillName, "skill", "",
		"name of a skill from the proposal to verify")
	cmd.Flags().Int64Var(&outputID, "output-id", 0,
		"specific llm_outputs row id (default: latest propose row)")
	return cmd
}

// verifyProposalOrAbort runs the Voyager-style critic gate before
// `propose add` writes anything. The critic's decision is cached
// under kind=propose_verify keyed by (outputID, skill.Name) — so a
// second run on the same proposal hits the cache for free, and the
// user can see the same concern again if they re-invoke without
// having to pay for another LLM call.
//
// Returns nil only when go_ahead=true. On refusal returns an error
// carrying the critic's concern + recommendation, formatted for
// the user to act on.
//
// Errors out of the cache lookup / LLM call don't refuse the apply
// silently — they propagate, so a transient network failure can be
// retried (or bypassed via --no-verify) rather than blocking forever.
func verifyProposalOrAbort(
	ctx context.Context,
	c *apiclient.Client,
	skill *prompts.ProposedSkill,
	outputID int64,
	newClient func() (llm.Client, error),
	out io.Writer,
) error {
	hash := proposeVerifyHash(outputID, skill.Name)

	// Cache: re-running apply on the same proposal is free.
	cached, err := c.LLMOutputByHash(ctx, string(store.LLMKindProposeVerify), hash)
	switch {
	case err == nil:
		var v prompts.ProposalVerification
		if jerr := json.Unmarshal([]byte(cached.Body), &v); jerr != nil {
			// Cached body is malformed — fall through to a fresh
			// call rather than refusing the apply on a parse bug.
			slog.Warn("propose verify: malformed cached body, re-running",
				"id", cached.ID, "err", jerr)
		} else {
			return reportVerification(out, &v, true)
		}
	case errors.Is(err, apiclient.ErrNotFound):
		// fall through
	default:
		return fmt.Errorf("propose verify: cache lookup: %w", err)
	}

	// Build prompt: skill + cited evidence digests + installed skills.
	// Filesystem walk against st.DB() for the per-cwd skill scan
	// stays — the api doesn't expose CollectInstalled, and the
	// scan is a read-only filesystem traversal that has no bearing
	// on the writer invariant.
	installed := loadInstalledSkills(ctx, c,
		time.Now().Add(-90*24*time.Hour).UnixMilli(), "propose verify")

	built, err := prompts.BuildVerifyProposal(prompts.VerifyProposalInputs{
		Skill:           *skill,
		InstalledSkills: installed,
	})
	if err != nil {
		return fmt.Errorf("propose verify: build prompt: %w", err)
	}

	client, err := newClient()
	if err != nil {
		return fmt.Errorf("propose verify: %w", err)
	}
	resp, err := client.Complete(ctx, built.Request)
	if err != nil {
		return fmt.Errorf("propose verify: LLM call: %w", err)
	}

	var verification prompts.ProposalVerification
	if err := parseToolResult(resp, prompts.ToolNameProposalVerify, &verification); err != nil {
		return fmt.Errorf("propose verify: %w", err)
	}

	body, err := marshalLLMBody(&verification)
	if err != nil {
		return fmt.Errorf("propose verify: marshal: %w", err)
	}
	saveReq := wire.SaveLLMOutputRequest{
		Kind:        string(store.LLMKindProposeVerify),
		Model:       resp.Model,
		PromptHash:  hash,
		Body:        body,
		CreatedAtMs: time.Now().UnixMilli(),
	}
	applyUsage(&saveReq, resp.Usage)
	if _, err := c.SaveLLMOutput(ctx, saveReq); err != nil {
		// Persisting failed but we have the verification — don't
		// re-pay for another call just because caching failed.
		// Log and proceed; user gets the decision they paid for.
		slog.Warn("propose verify: persist failed (decision still applied)", "err", err)
	}

	return reportVerification(out, &verification, false)
}

// reportVerification prints the critic's decision and returns an
// error iff go_ahead=false so the caller short-circuits before
// writing files. fromCache flips the prefix between "(verified
// fresh)" and "(verified, cached)" so the user can tell whether
// they paid for a new LLM call.
func reportVerification(out io.Writer, v *prompts.ProposalVerification, fromCache bool) error {
	source := "fresh"
	if fromCache {
		source = "cached"
	}
	if v.GoAhead {
		_, _ = fmt.Fprintf(out, "verify: ✓ critic approved (%s)\n", source)
		return nil
	}
	// Refusal: surface concern + recommendation as an error so the
	// caller's RunE returns non-zero. Format keeps the structure
	// readable on a terminal AND parseable by humans.
	severity := v.Severity
	if severity == "" {
		severity = "unspecified"
	}
	return fmt.Errorf("verify: ✗ critic refused (%s, severity=%s):\n  concern:        %s\n  recommendation: %s\n  → fix the proposal or pass --no-verify to apply anyway",
		source, severity, v.Concern, v.Recommendation)
}

// proposeVerifyHash is the cache key for one verification: a
// deterministic hash over (outputID, skill.Name). Unlike the
// other LLM-output kinds we don't include the prompt body — the
// (proposal-id, skill-name) pair already uniquely identifies the
// thing being verified, and forcing a re-run on prompt-text drift
// would defeat the cache without adding correctness.
func proposeVerifyHash(outputID int64, skillName string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(outputID, 10)))
	h.Write([]byte{'\x00'})
	h.Write([]byte(skillName))
	return hex.EncodeToString(h.Sum(nil))
}
