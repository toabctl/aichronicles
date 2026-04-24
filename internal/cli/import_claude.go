package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/ingest"
	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
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

// maxClaudeLineBytes caps a single transcript line. Real assistant
// turns can be huge; 16 MiB matches the daemon's ingest cap.
const maxClaudeLineBytes = 16 << 20

func newImportClaudeTranscriptsCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "import-claude-transcripts [path]",
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
			"works too.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := defaultClaudeProjectsDir()
			if len(args) == 1 {
				target = args[0]
			}

			resolvedDB := dbPath
			if resolvedDB == "" {
				p, err := paths.StorePath()
				if err != nil {
					return err
				}
				resolvedDB = p
			}
			s, err := store.Open(resolvedDB)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			report, err := ImportClaudeTranscripts(target, s, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
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
// ingests every conversational transcript line. warnOut receives one
// line per skipped-with-warning entry (missing uuid, invalid JSON)
// so the user can grep the original transcript.
func ImportClaudeTranscripts(target string, s *store.Store, warnOut io.Writer) (ClaudeImportReport, error) {
	start := time.Now()
	report := ClaudeImportReport{}

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

	for _, path := range files {
		if err := importClaudeFile(path, s, &report, warnOut); err != nil {
			report.DurationMS = time.Since(start).Milliseconds()
			return report, fmt.Errorf("import %s: %w", path, err)
		}
	}
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

func importClaudeFile(path string, s *store.Store, report *ClaudeImportReport, warnOut io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	report.FilesRead++

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), maxClaudeLineBytes)

	var lineNum int
	for sc.Scan() {
		lineNum++
		report.LinesRead++

		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var entry claudeEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			report.Invalid++
			importWarn(warnOut, fmt.Sprintf("invalid JSON at %s:%d: %v", path, lineNum, err))
			continue
		}

		if _, canonical := claudeCanonicalTypes[entry.Type]; !canonical {
			// Claude-internal bookkeeping row (see claudeCanonicalTypes
			// comment). Silent skip — not surprise, not error.
			continue
		}

		if entry.UUID == "" {
			report.SkippedMissingUUID++
			importWarn(warnOut, fmt.Sprintf("conversational row without uuid at %s:%d (type=%s)", path, lineNum, entry.Type))
			continue
		}
		if _, err := uuid.Parse(entry.UUID); err != nil {
			report.SkippedMissingUUID++
			importWarn(warnOut, fmt.Sprintf("malformed uuid at %s:%d: %q", path, lineNum, entry.UUID))
			continue
		}

		env, rawForStore, err := transcriptEntryToEnvelope(&entry, line)
		if err != nil {
			report.Invalid++
			importWarn(warnOut, fmt.Sprintf("convert %s:%d: %v", path, lineNum, err))
			continue
		}
		if err := env.Validate(); err != nil {
			report.Invalid++
			importWarn(warnOut, fmt.Sprintf("envelope validate %s:%d: %v", path, lineNum, err))
			continue
		}

		deduped, err := importOneEnvelope(s, env, rawForStore)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNum, err)
		}
		if deduped {
			report.Deduped++
		} else {
			report.Imported++
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	return nil
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
			&ingest.Tool{Name: name, NameRaw: name, CallID: callID}
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

// importWarn writes a single-line warning to w. The Fprintln error
// return is intentionally discarded — if stderr itself is broken
// there's nothing useful to do.
func importWarn(w io.Writer, args ...any) {
	parts := append([]any{"aichronicles import-claude:"}, args...)
	_, _ = fmt.Fprintln(w, parts...)
}

// importOneEnvelope runs one envelope through store.IngestEnvelope in
// its own transaction. Shared with the events.jsonl importer in
// import_jsonl.go via importOne, but the two are kept separate so
// schema drift for either format is isolated.
func importOneEnvelope(s *store.Store, env *ingest.Envelope, raw []byte) (bool, error) {
	tx, err := s.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	tsServer := time.Now().UTC().UnixMilli()
	deduped, err := store.IngestEnvelope(tx, env, raw, tsServer)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return deduped, nil
}
