package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/events/sources/claude"
)

func newImportClaudeCmd() *cobra.Command {
	var sockFlag string
	cmd := &cobra.Command{
		Use:   "import-claude <path>",
		Short: "Walk Claude Code .jsonl transcripts and stream them into aichronicles-api",
		Long: "Walks the directory at <path> looking for Claude Code session\n" +
			"transcripts (.jsonl files) and streams every conversational\n" +
			"line through POST /v1/import. The api applies server-side\n" +
			"redaction, runs the extractor registry, and writes through the\n" +
			"SQLite Sink — same path as live ingest. Idempotent on event_id.\n\n" +
			"<path> may be a single .jsonl or a directory; directories are\n" +
			"walked recursively. Use this once after upgrading to backfill\n" +
			"the user's prior Claude Code history into the store.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openAPIClient(sockFlag)
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(),
				&slog.HandlerOptions{Level: slog.LevelInfo})).With("cmd", "aichronicles import-claude")
			report, err := ImportClaudeTranscripts(cmd.Context(), c, args[0], log)
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

// ClaudeImportReport summarises an ImportClaudeTranscripts run.
// Stats split between Source-side (file/line walk + parser
// rejects) and api-side (Imported / Deduped / Invalid).
type ClaudeImportReport struct {
	FilesRead          int
	LinesRead          int
	SkippedMissingUUID int
	Invalid            int
	Imported           int
	Deduped            int
	DurationMS         int64
}

func (r ClaudeImportReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "files read:           %d\n", r.FilesRead)
	fmt.Fprintf(&b, "lines read:           %d\n", r.LinesRead)
	fmt.Fprintf(&b, "imported:             %d\n", r.Imported)
	fmt.Fprintf(&b, "deduped:              %d (already in store by event_id)\n", r.Deduped)
	fmt.Fprintf(&b, "skipped missing uuid: %d (conversational rows without uuid — format drift)\n", r.SkippedMissingUUID)
	fmt.Fprintf(&b, "invalid:              %d (malformed JSON, missing fields, or api-side rejects)\n", r.Invalid)
	fmt.Fprintf(&b, "duration_ms:          %d", r.DurationMS)
	return b.String()
}

// ImportClaudeTranscripts walks target and streams every
// conversational transcript line into POST /v1/import. The
// claude.JSONLSource handles file walking, line parsing, and
// per-line skips with structured logging; the api applies
// redaction + extractors + storage.
//
// Implemented as a goroutine that writes NDJSON to an io.Pipe
// while apiclient.Import reads from the other end — one open
// transcript collection at a time, no full-corpus buffer in
// memory.
func ImportClaudeTranscripts(ctx context.Context, c *apiclient.Client, target string, log *slog.Logger) (ClaudeImportReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	start := time.Now()
	report := ClaudeImportReport{}

	src := &claude.JSONLSource{
		Root:   target,
		Logger: log,
	}

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	// Producer goroutine: walk the Source, marshal each Envelope
	// as one NDJSON line, write to the pipe. Closes the pipe
	// writer with the first error so the consumer (api.Import)
	// sees EOF + the error via pr.Read.
	go func() {
		err := streamClaudeNDJSON(ctx, src, pw, log)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	stats, err := c.Import(ctx, pr)
	report.FilesRead = src.Stats.FilesRead
	report.LinesRead = src.Stats.LinesRead
	report.SkippedMissingUUID = src.Stats.SkippedMissingUUID
	// Source-side invalid + api-side invalid are both real
	// rejection signals; sum them so the operator sees the
	// total.
	report.Invalid = src.Stats.Invalid + stats.Invalid
	report.Imported = stats.Imported
	report.Deduped = stats.Deduped
	report.DurationMS = time.Since(start).Milliseconds()
	return report, err
}

// streamClaudeNDJSON walks src and writes one NDJSON line per
// envelope to w. Returns on the first write error or when src is
// exhausted. Produces the canonical post-redaction-pipeline
// envelope shape (server re-redacts on import; the line we ship
// here is the unredacted Source output).
func streamClaudeNDJSON(ctx context.Context, src *claude.JSONLSource, w io.Writer, log *slog.Logger) error {
	enc := json.NewEncoder(w)
	for ev, err := range src.Events(ctx) {
		if err != nil {
			log.Warn("import-claude: source error", "err", err)
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

// _ keeps os imported for the cobra flag-resolution branches
// that may surface again as the cli grows. Removing it would
// require a touch in newImportClaudeCmd.
var _ = os.Stdout
