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
	"github.com/toabctl/aichronicles/pkg/events/sources/gemini"
	"github.com/toabctl/aichronicles/pkg/redact"
)

func newImportGeminiCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "import-gemini [path]",
		Short: "Import gemini-cli session JSON files into the store",
		Long: "Walks one or more gemini-cli session files (one JSON per\n" +
			"conversation, written under ~/.gemini/tmp/<project>/chats/) and\n" +
			"ingests every message — user prompt, assistant turn, tool call,\n" +
			"tool result — as a canonical envelope.\n\n" +
			"event_id is the message's own UUID for user / assistant turns;\n" +
			"tool_use and tool_result events synthesize an id by\n" +
			"UUIDv5(namespace, parentMessageID + tool_call_id + suffix). All\n" +
			"three are stable across re-imports so the operation is\n" +
			"idempotent.\n\n" +
			"path defaults to ~/.gemini/tmp (gemini-cli's per-project root).\n" +
			"A specific session-*.json file or any directory below the root\n" +
			"works too.\n\n" +
			"Trust model: like import-claude, this bypasses the daemon. Edge\n" +
			"redaction runs in-process; anything else the daemon enforces\n" +
			"does not.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := defaultGeminiTmpDir()
			if len(args) == 1 {
				target = args[0]
			}

			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			log := newImportGeminiLogger(cmd.ErrOrStderr())
			report, err := ImportGeminiTranscripts(cmd.Context(), target, s, log)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

// newImportGeminiLogger wraps stderr in a slog.Logger pinned to
// the import-gemini command name.
func newImportGeminiLogger(stderr io.Writer) *slog.Logger {
	h := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With("cmd", "aichronicles import-gemini")
}

// defaultGeminiTmpDir returns ~/.gemini/tmp — gemini-cli's
// per-project session-state root.
func defaultGeminiTmpDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "tmp")
}

// GeminiImportReport summarizes an ImportGeminiTranscripts run.
type GeminiImportReport struct {
	FilesRead    int
	MessagesRead int
	Imported     int
	Deduped      int
	Invalid      int
	DurationMS   int64
}

func (r GeminiImportReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "files read:    %d\n", r.FilesRead)
	fmt.Fprintf(&b, "messages read: %d\n", r.MessagesRead)
	fmt.Fprintf(&b, "imported:      %d (envelopes; an assistant turn with N tool calls produces 1+2N envelopes)\n", r.Imported)
	fmt.Fprintf(&b, "deduped:       %d (already in store by event_id)\n", r.Deduped)
	fmt.Fprintf(&b, "invalid:       %d (malformed JSON or missing required fields)\n", r.Invalid)
	fmt.Fprintf(&b, "duration_ms:   %d", r.DurationMS)
	return b.String()
}

// ImportGeminiTranscripts walks target and ingests every parseable
// gemini-cli session file via the events.Pipeline. The
// TranscriptSource fans out one message into one or more envelopes
// (assistant turns with tool calls become 1+2N envelopes); the
// BufferedSink amortises SQLite fsync cost across them.
func ImportGeminiTranscripts(ctx context.Context, target string, s *store.Store, log *slog.Logger) (GeminiImportReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	start := time.Now()
	report := GeminiImportReport{}

	src := &gemini.TranscriptSource{
		Root:   target,
		Logger: log,
	}
	sink := store.NewBufferedSink(s, store.BufferedSinkOpts{})
	defer func() { _ = sink.Close() }()

	pipeline := events.Pipeline{
		Sink:       sink,
		Extractors: events.DefaultExtractors(),
		Redactor:   events.NewScannerRedactor(redact.Default()),
		Logger:     log,
	}

	stats, err := pipeline.Run(ctx, src)
	report.FilesRead = src.Stats.FilesRead
	report.MessagesRead = src.Stats.MessagesRead
	report.Invalid = src.Stats.Invalid
	report.Imported = stats.Processed - stats.Deduped
	report.Deduped = stats.Deduped
	report.DurationMS = time.Since(start).Milliseconds()
	return report, err
}
