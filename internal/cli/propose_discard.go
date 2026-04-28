package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// newProposeDiscardCmd is the AutoSkill maintenance action 'discard'
// command: mark a candidate as actively rejected so future propose
// runs see "the user declined this" and bias away from re-suggesting
// near-duplicates. No filesystem I/O — the only side effect is the
// skill_candidates row's decision flipping to MaintenanceDiscard.
//
// Distinct from "the user did nothing" (decision IS NULL): an
// explicit discard is a stronger negative signal. The propose
// system prompt's prior-proposals stanza already treats not-acted-on
// candidates as a prior; an explicit discard tightens that signal.
func newProposeDiscardCmd() *cobra.Command {
	var (
		dbPath    string
		skillName string
		outputID  int64
	)
	cmd := &cobra.Command{
		Use:   "discard --skill <name>",
		Short: "Mark a proposed skill as discarded (AutoSkill action 'discard')",
		Long: "Records the AutoSkill (Yang et al., 2026 — arXiv:2603.01145)\n" +
			"maintenance action 'discard' on a candidate the user does\n" +
			"not want — neither added to disk nor merged into anything.\n" +
			"Future propose runs see this as an explicit rejection and\n" +
			"will bias away from re-suggesting near-duplicates.\n\n" +
			"No filesystem I/O. The skill_candidates row's decision\n" +
			"flips to 'discard' with the current timestamp; nothing\n" +
			"on disk changes.",
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

			return discardProposedSkill(cmd.Context(), s, result, output.ID, skillName, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	cmd.Flags().StringVar(&skillName, "skill", "",
		"name of a skill from the proposal to discard")
	cmd.Flags().Int64Var(&outputID, "output-id", 0,
		"specific llm_outputs row id (default: latest propose row)")
	return cmd
}

// discardProposedSkill flips the candidate's lifecycle decision to
// MaintenanceDiscard. The skill_candidates row must already exist
// (RecordSkillCandidate runs at extraction time); a missing row
// auto-inserts via the same fallback path the add and merge flows
// use, so a discard called against a candidate predating the
// lifecycle index still records.
func discardProposedSkill(
	ctx context.Context,
	st *store.Store,
	r *prompts.ProposalResult,
	outputID int64,
	name string,
	out io.Writer,
) error {
	candidate, err := findProposedSkill(r, name)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	if merr := store.MarkSkillCandidateDiscarded(ctx, st.DB(), outputID, candidate.Name, now); merr != nil {
		if errors.Is(merr, store.ErrSkillCandidateNotFound) {
			if rerr := store.RecordSkillCandidate(ctx, st.DB(), outputID, candidate.Name, now); rerr != nil {
				return fmt.Errorf("auto-insert candidate row: %w", rerr)
			}
			if rerr := store.MarkSkillCandidateDiscarded(ctx, st.DB(), outputID, candidate.Name, now); rerr != nil {
				return fmt.Errorf("mark discarded after auto-insert: %w", rerr)
			}
		} else {
			return fmt.Errorf("mark discarded: %w", merr)
		}
	}
	_, _ = fmt.Fprintf(out, "discarded %s — recorded as a rejection signal for future propose runs\n", candidate.Name)
	return nil
}
