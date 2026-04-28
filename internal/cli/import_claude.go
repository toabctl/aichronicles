package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// claudeCanonicalTypes are the Claude transcript entry types that
// carry conversational content we convert to canonical envelopes.
//
// Every other type we've observed in ~/.claude/projects/*.jsonl is
// Claude-internal bookkeeping that only exists in the on-disk
// transcript, NOT in the hook event stream. None of them unlock
// queries we can't already answer from the canonical events, so we
// skip them silently rather than storing noise:
//
//   - file-history-snapshot : Claude's file-undo snapshots. Same
//     file edits are already captured via our Edit/Write tool_use
//     events and the file_path extractor. Pure duplication.
//   - permission-mode       : "user switched to bypass mode" marker.
//     Niche, low-signal, no text content to search.
//   - queue-operation       : internal enqueue/dequeue markers. No
//     user-visible semantics.
//   - attachment            : metadata about files attached to a
//     user turn (but not the file contents). The subsequent
//     user_prompt usually references the attachment anyway.
//   - last-prompt           : mirrors the most recent user_prompt.
//     Straight duplicate of an event we already import.
var claudeCanonicalTypes = map[string]struct{}{
	"user":      {},
	"assistant": {},
	"system":    {},
}

// maxClaudeLineBytes is a sanity bound on a single transcript line.
// Real assistant turns can be huge (50 MB+ is not unheard of when a
// tool result gets inlined), so we read line-by-line via a bufio
// Reader instead of a Scanner — the Scanner's token cap is exactly
// the failure mode this constant defends against when it does trip.
// Any line above this is logged + counted + skipped rather than
// aborting the whole import.
const maxClaudeLineBytes = 128 << 20

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

			resolvedDB, err := paths.ResolveStorePath(dbPath)
			if err != nil {
				return err
			}
			s, err := store.Open(resolvedDB)
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
// package (ingest, summarize, propose, reflect, mcp-serve). Tests
// pass a custom logger writing to a bytes.Buffer.
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
// Non-conversational entry types (see claudeCanonicalTypes comment)
// are skipped silently; only events the importer did not expect are
// counted as invalid or missing-uuid.
type ClaudeImportReport struct {
	FilesRead          int
	LinesRead          int
	Imported           int
	Deduped            int
	SkippedMissingUUID int // conversational row with no/bad uuid (surfaces format drift)
	Invalid            int // malformed JSON or missing required fields
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

// ImportClaudeTranscripts walks target (file or directory) and
// ingests every conversational transcript line. log receives one
// Warn record per skipped-with-warning entry (missing uuid, invalid
// JSON) plus Info-level progress (one per file). The ctx is
// propagated to every store write so Ctrl-C stops an import between
// chunks rather than after the current file.
//
// Envelopes are streamed into one envelopeBatcher shared across all
// files so chunk fsync cost amortises across file boundaries — a
// directory of small transcripts no longer pays one commit per
// short file plus one per row.
func ImportClaudeTranscripts(ctx context.Context, target string, s *store.Store, log *slog.Logger) (ClaudeImportReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	start := time.Now()
	report := ClaudeImportReport{}
	batcher := newEnvelopeBatcher(s)

	info, err := os.Stat(target)
	if err != nil {
		report.DurationMS = time.Since(start).Milliseconds()
		return report, fmt.Errorf("stat %s: %w", target, err)
	}

	var files []string
	if info.IsDir() {
		err := filepath.WalkDir(target, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".jsonl") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			report.DurationMS = time.Since(start).Milliseconds()
			return report, fmt.Errorf("walk %s: %w", target, err)
		}
	} else {
		files = []string{target}
	}

	// Progress header: emit the file count up-front so the user has
	// a sense of total work. One Info record per file follows below.
	// Stdout stays reserved for the final report so callers can pipe
	// it; the logger writes structured records to stderr.
	if len(files) > 0 {
		log.Info("import starting", "files", len(files))
	}

	for i, path := range files {
		fileStart := time.Now()
		linesBefore := report.LinesRead
		if err := importClaudeFile(ctx, path, batcher, &report, log); err != nil {
			// Make sure any buffered envelopes from earlier files
			// land before we return — the contract is "progress
			// up to the last completed chunk is durable".
			_ = batcher.Flush(ctx)
			report.Imported = batcher.Imported()
			report.Deduped = batcher.Deduped()
			report.DurationMS = time.Since(start).Milliseconds()
			return report, fmt.Errorf("import %s: %w", path, err)
		}
		log.Info("file done",
			"index", i+1,
			"total", len(files),
			"path", shortPath(path),
			"lines", report.LinesRead-linesBefore,
			"elapsed", time.Since(fileStart),
		)
	}

	if err := batcher.Flush(ctx); err != nil {
		report.Imported = batcher.Imported()
		report.Deduped = batcher.Deduped()
		report.DurationMS = time.Since(start).Milliseconds()
		return report, fmt.Errorf("flush: %w", err)
	}

	report.Imported = batcher.Imported()
	report.Deduped = batcher.Deduped()
	report.DurationMS = time.Since(start).Milliseconds()
	return report, nil
}

// claudeEntry is the subset of a transcript line we care about. The
// raw line is preserved separately and stored in envelope_json so
// every source-native field survives without us modeling each one.
type claudeEntry struct {
	Type      string         `json:"type"`
	UUID      string         `json:"uuid"`
	SessionID string         `json:"sessionId"`
	Timestamp string         `json:"timestamp"`
	CWD       string         `json:"cwd"`
	Version   string         `json:"version"`
	Message   *claudeMessage `json:"message"`
	ToolUseID string         `json:"toolUseID"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model"`
}

func importClaudeFile(ctx context.Context, path string, batcher *envelopeBatcher, report *ClaudeImportReport, log *slog.Logger) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	report.FilesRead++

	// bufio.Reader, not Scanner: real Claude transcripts legitimately
	// carry multi-tens-of-MB lines when a large tool result is inlined
	// into an assistant turn, and Scanner's token cap surfaces those
	// as a fatal "token too long" instead of a per-line skip. Reader's
	// ReadBytes has no such cap — memory is bounded by the longest
	// line, which we further guard with maxClaudeLineBytes below.
	br := bufio.NewReaderSize(f, 1<<20)

	var lineNum int
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			lineNum++
			report.LinesRead++
			if err := processClaudeLine(ctx, batcher, path, lineNum, line, report, log); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
	}
}

// processClaudeLine handles one transcript line. Returns a non-nil
// error ONLY for storage-level failures; every format / validation
// issue is counted into report and reported via warnOut so one bad
// line never aborts the whole import.
//
// Lines that pass validation are queued on the batcher rather than
// committed individually — the batcher owns the imported / deduped
// counters, and ImportClaudeTranscripts copies them onto the report
// once the trailing chunk has been flushed.
func processClaudeLine(ctx context.Context, batcher *envelopeBatcher, path string, lineNum int, line []byte, report *ClaudeImportReport, log *slog.Logger) error {
	if len(line) > maxClaudeLineBytes {
		report.Invalid++
		log.Warn("line too large",
			"path", path, "line", lineNum,
			"bytes", len(line), "cap", maxClaudeLineBytes)
		return nil
	}
	line = bytesStripTrailingNewline(line)
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}

	var entry claudeEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		report.Invalid++
		log.Warn("invalid JSON", "path", path, "line", lineNum, "err", err)
		return nil
	}

	if _, canonical := claudeCanonicalTypes[entry.Type]; !canonical {
		// Claude-internal bookkeeping row — silent skip.
		return nil
	}

	if entry.UUID == "" {
		report.SkippedMissingUUID++
		log.Warn("conversational row without uuid",
			"path", path, "line", lineNum, "type", entry.Type)
		return nil
	}
	if _, err := uuid.Parse(entry.UUID); err != nil {
		report.SkippedMissingUUID++
		log.Warn("malformed uuid",
			"path", path, "line", lineNum, "uuid", entry.UUID)
		return nil
	}

	env, rawForStore, err := transcriptEntryToEnvelope(&entry, line)
	if err != nil {
		report.Invalid++
		log.Warn("convert envelope",
			"path", path, "line", lineNum, "err", err)
		return nil
	}
	if err := env.Validate(); err != nil {
		report.Invalid++
		log.Warn("envelope validate",
			"path", path, "line", lineNum, "err", err)
		return nil
	}

	if err := batcher.Add(ctx, env, rawForStore); err != nil {
		return fmt.Errorf("%s:%d: %w", path, lineNum, err)
	}
	return nil
}

// bytesStripTrailingNewline drops a single trailing "\n" or "\r\n"
// from line without allocating a new slice. ReadBytes includes the
// terminator; callers want the payload.
func bytesStripTrailingNewline(line []byte) []byte {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
	}
	if n > 0 && line[n-1] == '\r' {
		n--
	}
	return line[:n]
}

// transcriptEntryToEnvelope converts one Claude transcript line into a
// canonical Envelope. The raw transcript line is stored verbatim in
// envelope_json via payload so no source-native field is lost.
//
// Kind derivation is per-line (1:1 mapping): an assistant entry that
// includes tool_use blocks becomes a tool_use event, not a separate
// assistant_message + tool_use pair. Users wanting that split can
// run a future re-derive pass; for MVP the 1:1 mapping keeps the
// event_id = uuid property intact.
func transcriptEntryToEnvelope(entry *claudeEntry, rawLine []byte) (*ingest.Envelope, []byte, error) {
	if entry.SessionID == "" {
		return nil, nil, errors.New("missing sessionId")
	}
	if entry.Timestamp == "" {
		return nil, nil, errors.New("missing timestamp")
	}
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		return nil, nil, fmt.Errorf("parse timestamp %q: %w", entry.Timestamp, err)
	}

	kind, role, contentText, tool := classifyClaudeEntry(entry)

	// Rebuild the raw bytes into our payload shape: it's just the
	// original line re-unmarshaled into a generic map so the daemon's
	// store.IngestEnvelope sees the same JSON it would from a hook.
	var payload map[string]any
	if err := json.Unmarshal(rawLine, &payload); err != nil {
		return nil, nil, fmt.Errorf("reparse line to payload: %w", err)
	}

	env := &ingest.Envelope{
		V:                  1,
		EventID:            entry.UUID,
		SourceAgent:        "claude-code",
		SourceAgentVersion: entry.Version,
		SourceSessionID:    entry.SessionID,
		Kind:               kind,
		Role:               role,
		TsSource:           ts,
		Cwd:                entry.CWD,
		Tool:               tool,
		ContentText:        contentText,
		Payload:            payload,
		Transport:          "import",
	}

	// Scrub before we serialize. The scrubbed envelope is what lands
	// in both events.content_text and raw_envelopes.envelope_json, so
	// a transcript that contains a pasted API key never reaches disk
	// in plain form. Idempotent on re-import (markers don't match).
	ingest.ApplyRedaction(env, redact.Default())

	// Re-marshal deterministically so raw_envelopes.envelope_json is
	// the *our-canonical-envelope* JSON, not Claude's wire format.
	// This matches the hook path (daemon stores what the CLI sent).
	out, err := json.Marshal(env)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return env, out, nil
}

// classifyClaudeEntry picks a canonical kind, role, content_text, and
// optional Tool from a transcript entry. Heuristics:
//
//   - user   + string content              → user_prompt
//   - user   + array content w/ tool_result → tool_result (first block)
//   - user   + array content (text only)   → user_prompt (concat text)
//   - assistant + array w/ tool_use        → tool_use (first block)
//   - assistant + array (text only)        → assistant_message (concat)
//   - system                               → system_message
func classifyClaudeEntry(entry *claudeEntry) (kind, role, contentText string, tool *ingest.Tool) {
	switch entry.Type {
	case "system":
		return "system_message", "system", stringContent(entry.Message), nil
	case "user":
		return classifyUserEntry(entry)
	case "assistant":
		return classifyAssistantEntry(entry)
	}
	// Caller should have filtered to canonical types; defensive default.
	return "unknown", "", stringContent(entry.Message), nil
}

func classifyUserEntry(entry *claudeEntry) (string, string, string, *ingest.Tool) {
	if entry.Message == nil || len(entry.Message.Content) == 0 {
		return "user_prompt", "user", "", nil
	}
	// content may be a bare string (plain user text) or an array of
	// content blocks (text / tool_result).
	var asString string
	if err := json.Unmarshal(entry.Message.Content, &asString); err == nil {
		return "user_prompt", "user", asString, nil
	}
	blocks := parseBlocks(entry.Message.Content)
	if first, ok := firstBlockOfType(blocks, "tool_result"); ok {
		callID, _ := first["tool_use_id"].(string)
		return "tool_result", "tool", flattenToolResultContent(first["content"]),
			&ingest.Tool{CallID: callID}
	}
	return "user_prompt", "user", joinTextBlocks(blocks), nil
}

func classifyAssistantEntry(entry *claudeEntry) (string, string, string, *ingest.Tool) {
	if entry.Message == nil || len(entry.Message.Content) == 0 {
		return "assistant_message", "assistant", "", nil
	}
	blocks := parseBlocks(entry.Message.Content)
	if first, ok := firstBlockOfType(blocks, "tool_use"); ok {
		name, _ := first["name"].(string)
		callID, _ := first["id"].(string)
		return "tool_use", "tool", name,
			&ingest.Tool{Name: name, CallID: callID}
	}
	return "assistant_message", "assistant", joinTextBlocks(blocks), nil
}

func parseBlocks(raw json.RawMessage) []map[string]any {
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

func firstBlockOfType(blocks []map[string]any, t string) (map[string]any, bool) {
	for _, b := range blocks {
		if typ, _ := b["type"].(string); typ == t {
			return b, true
		}
	}
	return nil, false
}

// joinTextBlocks concatenates "text" and "thinking" blocks' text for
// FTS indexing. Tool-call structures aren't flattened here — their
// metadata lives on ingest.Tool.
func joinTextBlocks(blocks []map[string]any) string {
	var b strings.Builder
	for _, block := range blocks {
		switch block["type"] {
		case "text":
			if s, ok := block["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		case "thinking":
			if s, ok := block["thinking"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// flattenToolResultContent renders whatever a tool returned into a
// searchable string. tool_result.content can be a string, an array of
// content blocks, or omitted.
func flattenToolResultContent(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, item := range x {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		// map or something unexpected — render as JSON so we at least
		// don't silently drop the data.
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func stringContent(m *claudeMessage) string {
	if m == nil || len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// If it's an array, fall through to block parsing.
	return joinTextBlocks(parseBlocks(m.Content))
}

// shortPath compresses a Claude transcript path for the progress
// log field. ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl is long;
// the encoded-cwd + filename is what the user can match to a
// session, so we drop the ~/.claude/projects/ prefix when present.
// Falls back to the full path when the prefix doesn't match (e.g.
// user passed an arbitrary directory).
func shortPath(p string) string {
	for _, prefix := range []string{
		os.Getenv("HOME") + "/.claude/projects/",
		"/.claude/projects/",
	} {
		if prefix != "" && strings.HasPrefix(p, prefix) {
			return p[len(prefix):]
		}
	}
	return p
}
