package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/events/sources/claude"
	"github.com/toabctl/aichronicles/pkg/redact"
)

func newImportClaudeCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "import-claude [path]",
		Short: "Import Claude Code's own ~/.claude transcripts into the store",
		Long: "Walks one or more Claude Code transcript files (*.jsonl) and\n" +
			"ingests each conversational line (user/assistant/system) as an\n" +
			"envelope. Claude-internal bookkeeping rows (file-history-snapshot,\n" +
			"permission-mode, queue-operation, attachment, last-prompt) are\n" +
			"silently skipped — they carry no content we search.\n\n" +
			"event_id is Claude's per-entry uuid verbatim so re-imports are\n" +
			"idempotent and the stored row is greppable against the source\n" +
			"transcript. A missing or malformed uuid on a conversational row\n" +
			"is logged loudly and counted — we surface format drift rather\n" +
			"than hide it behind a synthesized ID.\n\n" +
			"path defaults to ~/.claude/projects. A specific file or directory\n" +
			"works too.\n\n" +
			"Trust model: import-claude bypasses the daemon. Edge redaction\n" +
			"runs in-process before each envelope is stored, but anything\n" +
			"the daemon would otherwise enforce — future origin signing,\n" +
			"rate limits, audit logging — does not run. Imports operate on\n" +
			"files you already trust enough to read locally.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := defaultClaudeProjectsDir()
			if len(args) == 1 {
				target = args[0]
			}

			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			log := newImportClaudeLogger(cmd.ErrOrStderr())
			report, err := ImportClaudeTranscripts(cmd.Context(), target, s, log)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

// newImportClaudeLogger wraps stderr in a slog.Logger so progress
// (Info) and per-line skips (Warn) emit structured records, matching
// the convention used by every other long-running CLI in this
// package. Tests pass a custom logger writing to a bytes.Buffer.
func newImportClaudeLogger(stderr io.Writer) *slog.Logger {
	h := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With("cmd", "aichronicles import-claude")
}

// defaultClaudeProjectsDir returns $HOME/.claude/projects — Claude
// Code's canonical transcript root.
func defaultClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ClaudeImportReport summarizes an ImportClaudeTranscripts run.
// Non-conversational entry types are skipped silently; only events
// the importer did not expect are counted as invalid or
// missing-uuid.
type ClaudeImportReport struct {
	FilesRead          int
	LinesRead          int
	Imported           int
	Deduped            int
	SkippedMissingUUID int
	Invalid            int
	DurationMS         int64
}

func (r ClaudeImportReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "files read:           %d\n", r.FilesRead)
	fmt.Fprintf(&b, "lines read:           %d\n", r.LinesRead)
	fmt.Fprintf(&b, "imported:             %d\n", r.Imported)
	fmt.Fprintf(&b, "deduped:              %d (already in store by event_id)\n", r.Deduped)
	fmt.Fprintf(&b, "skipped missing uuid: %d (conversational rows without uuid — format drift)\n", r.SkippedMissingUUID)
	fmt.Fprintf(&b, "invalid:              %d (malformed JSON or missing required fields)\n", r.Invalid)
	fmt.Fprintf(&b, "duration_ms:          %d", r.DurationMS)
	return b.String()
}

// ImportClaudeTranscripts walks target and ingests every
// conversational transcript line via the events.Pipeline. The
// JSONLSource handles file walking, line parsing, and per-line
// skips with structured logging; the BufferedSink amortises
// SQLite fsync cost across many envelopes.
//
// log receives one Warn record per skipped-with-warning line
// (missing uuid, invalid JSON) and one Info per file when not
// nil. ctx is propagated through both source and sink so Ctrl-C
// stops between chunks rather than after the current file.
func ImportClaudeTranscripts(ctx context.Context, target string, s *store.Store, log *slog.Logger) (ClaudeImportReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	start := time.Now()
	report := ClaudeImportReport{}

	src := &claude.JSONLSource{
		Root:     target,
		Redactor: events.NewScannerRedactor(redact.Default()),
		Logger:   log,
	}
	sink := store.NewBufferedSink(s, store.BufferedSinkOpts{})
	defer func() { _ = sink.Close() }()

	pipeline := events.Pipeline{
		Sink:             sink,
		Extractors:       events.DefaultExtractors(),
		RequireRedaction: true,
		Logger:           log,
	}

	_, err := pipeline.Run(ctx, src)
	report.FilesRead = src.Stats.FilesRead
	report.LinesRead = src.Stats.LinesRead
	report.SkippedMissingUUID = src.Stats.SkippedMissingUUID
	report.Invalid = src.Stats.Invalid
	// Imported/Deduped come from the BufferedSink's running totals
	// (Pipeline.Stats can't track dedup for buffered sinks since
	// Write returns synthetic Result values pre-flush).
	report.Imported = sink.Imported()
	report.Deduped = sink.Deduped()
	report.DurationMS = time.Since(start).Milliseconds()
	return report, err
}
