package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/toabctl/aichronicles/internal/skills"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// verifyProposalOrAbort runs the Voyager-style critic gate before
// `propose apply` writes anything. The critic's decision is cached
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
	st *store.Store,
	skill *prompts.ProposedSkill,
	outputID int64,
	newClient func() (llm.Client, error),
	out io.Writer,
) error {
	hash := proposeVerifyHash(outputID, skill.Name)

	// Cache: re-running apply on the same proposal is free.
	cached, err := store.LoadLLMOutputByHash(ctx, st.DB(), store.LLMKindProposeVerify, hash)
	if err != nil {
		return fmt.Errorf("propose verify: cache lookup: %w", err)
	}
	if cached != nil {
		var v prompts.ProposalVerification
		if jerr := json.Unmarshal([]byte(cached.Body), &v); jerr != nil {
			// Cached body is malformed — fall through to a fresh
			// call rather than refusing the apply on a parse bug.
			slog.Warn("propose verify: malformed cached body, re-running",
				"id", cached.ID, "err", jerr)
		} else {
			return reportVerification(out, &v, true)
		}
	}

	// Build prompt: skill + cited evidence digests + installed skills.
	installed, err := skills.CollectInstalled(ctx, st.DB(),
		time.Now().Add(-90*24*time.Hour).UnixMilli())
	if err != nil {
		// Non-fatal: critic still runs, just without the
		// installed-skills enrichment. A propose run that just
		// finished probably has those baked into its prompt, so
		// the critic can still reason about duplicates from
		// what's in the proposal.
		slog.Warn("propose verify: skipping installed-skills enrichment", "err", err)
	}

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
	if _, err := persistSummary(ctx, st, &persistInput{
		kind:       store.LLMKindProposeVerify,
		hash:       hash,
		model:      resp.Model,
		inputToks:  resp.Usage.InputTokens,
		outputToks: resp.Usage.OutputTokens,
		body:       body,
	}); err != nil {
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
