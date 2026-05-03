package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/pkg/events/sources/gemini"
)

func newImportGeminiCmd() *cobra.Command {
	var sockFlag string
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

			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			log := newImportGeminiLogger(cmd.ErrOrStderr())
			report, err := ImportGeminiTranscripts(cmd.Context(), c, target, log)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&sockFlag, "socket", "",
		"aichronicles-api UDS path (overrides $AICHRONICLES_API_SOCKET)")
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

// ImportGeminiTranscripts walks target and streams every
// parseable gemini-cli session message into POST /v1/import.
// The TranscriptSource fans out one message into one or more
// envelopes (assistant turns with tool calls become 1+2N
// envelopes); each envelope becomes one NDJSON line on the wire.
// The api applies server-side redaction and runs the extractor
// registry — same path as live ingest. Idempotent on event_id.
func ImportGeminiTranscripts(ctx context.Context, c *apiclient.Client, target string, log *slog.Logger) (GeminiImportReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	start := time.Now()
	report := GeminiImportReport{}

	src := &gemini.TranscriptSource{
		Root:   target,
		Logger: log,
	}

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	go func() {
		err := streamGeminiNDJSON(ctx, src, pw, log)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	stats, err := c.Import(ctx, pr)
	report.FilesRead = src.Stats.FilesRead
	report.MessagesRead = src.Stats.MessagesRead
	report.Invalid = src.Stats.Invalid + stats.Invalid
	report.Imported = stats.Imported
	report.Deduped = stats.Deduped
	report.DurationMS = time.Since(start).Milliseconds()
	return report, err
}

// streamGeminiNDJSON walks src and writes one NDJSON line per
// envelope to w. Mirrors streamClaudeNDJSON; lives next to
// import_gemini.go's caller because TranscriptSource yields its
// own Event shape and the file walks differ subtly between the
// two source types.
func streamGeminiNDJSON(ctx context.Context, src *gemini.TranscriptSource, w io.Writer, log *slog.Logger) error {
	enc := json.NewEncoder(w)
	for ev, err := range src.Events(ctx) {
		if err != nil {
			log.Warn("import-gemini: source error", "err", err)
			continue
		}
		if ev.Envelope == nil {
			continue
		}
		if encErr := enc.Encode(ev.Envelope); encErr != nil {
			return encErr
		}
	}
	return nil
}
