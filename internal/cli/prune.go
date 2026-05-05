package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/pkg/api"
)

// defaultPruneAge is the lower bound the CLI applies when
// `--older-than` is unset. Six months balances "keep recent
// history searchable" against "single-developer DBs grow without
// bound." Easy to override.
const defaultPruneAge = 180 * 24 * time.Hour

func newPruneCmd() *cobra.Command {
	var (
		olderThan      time.Duration
		yes            bool
		includeLLMOuts bool
		sockFlag       string
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete sessions (and everything they own) older than --older-than",
		Long: "Removes every session whose ended_at_ms is older than --older-than\n" +
			"and cascades to its raw_envelopes / events / extractions / events_fts\n" +
			"rows. Active sessions (ended_at NULL) are protected, regardless of how\n" +
			"old started_at is.\n\n" +
			"Cached LLM outputs (summaries, reflections, propose drafts) survive by\n" +
			"default — their session_id is set NULL via the schema's ON DELETE\n" +
			"SET NULL, so they remain as historical record without a parent. Pass\n" +
			"--include-llm-outputs to drop those too.\n\n" +
			"Default is dry-run: nothing is written until you pass --yes. Run\n" +
			"`aichronicles vacuum` afterwards to reclaim freelist pages on disk.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			window := olderThan
			if window <= 0 {
				window = defaultPruneAge
			}
			cutoff := time.Now().Add(-window).UnixMilli()

			resp, err := c.Prune(cmd.Context(), api.PruneRequest{
				CutoffMs:          cutoff,
				IncludeLLMOutputs: includeLLMOuts,
				DryRun:            !yes,
			})
			if err != nil {
				return fmt.Errorf("prune: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), formatPruneResponse(resp, window))
			return nil
		},
	}
	addFlexDurationFlag(cmd, &olderThan, "older-than", defaultPruneAge,
		"prune sessions whose ended_at is older than this (e.g. 30d, 180d, 24h)")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"actually delete; without --yes the command runs as dry-run")
	cmd.Flags().BoolVar(&includeLLMOuts, "include-llm-outputs", false,
		"also delete llm_outputs rows older than the cutoff (summaries, reflections)")
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	return cmd
}

// formatPruneResponse renders the human-readable text the CLI
// prints after a (dry-)prune. Same shape as the legacy
// formatPruneReport but driven by the wire api.PruneResponse so
// the renderer stays internal/store-free.
func formatPruneResponse(r api.PruneResponse, window time.Duration) string {
	var b strings.Builder
	verb := "deleted"
	if r.DryRun {
		verb = "would delete (dry-run; pass --yes to commit)"
	}
	cutoff := time.UnixMilli(r.CutoffMs).UTC().Format("2006-01-02 15:04 UTC")
	fmt.Fprintf(&b, "prune (older than %s, cutoff %s) — %s:\n", humanDuration(window), cutoff, verb)
	fmt.Fprintf(&b, "  sessions:       %d\n", r.Sessions)
	fmt.Fprintf(&b, "  raw_envelopes:  %d\n", r.RawEnvelopes)
	fmt.Fprintf(&b, "  events:         %d  (cascade)\n", r.Events)
	fmt.Fprintf(&b, "  extractions:    %d  (cascade)\n", r.Extractions)
	if r.LLMOutputs > 0 {
		fmt.Fprintf(&b, "  llm_outputs:    %d  (--include-llm-outputs)\n", r.LLMOutputs)
	}
	if !r.DryRun {
		fmt.Fprintln(&b, "Run `aichronicles vacuum` to reclaim freelist pages on disk.")
	}
	return b.String()
}

func newVacuumCmd() *cobra.Command {
	var (
		yes      bool
		sockFlag string
	)
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Compact the SQLite store and truncate the WAL",
		Long: "Runs PRAGMA wal_checkpoint(TRUNCATE) followed by VACUUM. The\n" +
			"checkpoint flushes pending WAL frames into the main DB so VACUUM\n" +
			"sees current state; VACUUM then rewrites the DB into a temp file\n" +
			"and renames it, releasing freelist pages back to the filesystem.\n\n" +
			"Caveats:\n" +
			"  - VACUUM blocks concurrent writers (readers in WAL mode are fine).\n" +
			"    The daemon is a writer; consider stopping it during a vacuum on\n" +
			"    a heavily-active store.\n" +
			"  - VACUUM needs ~2× the DB size in free disk during the rewrite.\n" +
			"  - Pass --yes to actually run; default is a no-op preview that\n" +
			"    prints the current page count.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			before, err := c.DBInfo(cmd.Context())
			if err != nil {
				return fmt.Errorf("page info: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"DB before vacuum: %s (%d pages × %d bytes)\n",
				humanBytes(before.Bytes), before.PageCount, before.PageSize)
			if !yes {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(dry-run; pass --yes to actually vacuum)")
				return nil
			}

			if err := c.Vacuum(cmd.Context()); err != nil {
				return fmt.Errorf("vacuum: %w", err)
			}

			after, err := c.DBInfo(cmd.Context())
			if err != nil {
				return fmt.Errorf("page info (after): %w", err)
			}
			delta := before.Bytes - after.Bytes
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"DB after vacuum:  %s (%d pages × %d bytes)\nreclaimed:        %s\n",
				humanBytes(after.Bytes), after.PageCount, after.PageSize,
				humanBytes(delta))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "actually vacuum; without --yes the command prints current size and exits")
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
	return cmd
}

// humanDuration renders a Duration as "Nd" when it's a whole
// number of days, falling back to time.Duration's default
// formatting (h/m/s) otherwise. Mirrors the day-shorthand the
// --since / --older-than flags accept on input.
func humanDuration(d time.Duration) string {
	const day = 24 * time.Hour
	if d > 0 && d%day == 0 {
		return fmt.Sprintf("%dd", int(d/day))
	}
	return d.String()
}

// humanBytes formats a byte count as KiB / MiB / GiB. Conservative
// (binary, two decimals) so prune/vacuum output reads consistently
// across machines.
func humanBytes(n int64) string {
	const (
		kib = 1024
		mib = kib * 1024
		gib = mib * 1024
	)
	switch {
	case n < kib:
		return fmt.Sprintf("%d B", n)
	case n < mib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	case n < gib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/gib)
	}
}
